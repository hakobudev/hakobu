package ops

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// Every minute hakobu records the CPU, memory, network and disk use of the
// server, of each app and worker and of its own services in telemetry.db:
// a minute for 48 hours, then five-minute averages and peaks for a month,
// in rings of fixed slots (migration 002_samples.sql).
const (
	minuteRes   = 60
	fiveMinRes  = 5 * 60
	minuteSlots = 48 * 60          // 48 hours
	fiveSlots   = 30 * 24 * 60 / 5 // 30 days
)

// MetricRanges are the spans the panel and MCP show usage over.
var MetricRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// HostTarget is the server's own usage.
const HostTarget = "host"

// reading is a container's counters and when they were read.
type reading struct {
	c  node.Counters
	at time.Time
}

// metricState is what the next minute's usage is worked out from.
type metricState struct {
	servers map[string]*serverUsage // by server name, "" for the panel's
	rolled  int64                   // the last five minutes summarized
}

// serverUsage is a server's last readings.
type serverUsage struct {
	containers map[string]reading // by container ID
	host       node.HostCounters
}

func (st *metricState) server(name string) *serverUsage {
	if st.servers == nil {
		st.servers = map[string]*serverUsage{}
	}
	if st.servers[name] == nil {
		st.servers[name] = &serverUsage{containers: map[string]reading{}}
	}
	return st.servers[name]
}

// hostTarget is what a server's own usage is recorded as.
func hostTarget(serverName string) string {
	if serverName == "" {
		return HostTarget
	}
	return HostTarget + ":" + serverName
}

// ServiceTarget is what hakobu's service name (postgres, cloudflared,
// buildkit) on a server is recorded as.
func ServiceTarget(name, serverName string) string {
	if serverName == "" {
		return "service:" + name
	}
	return "service:" + name + ":" + serverName
}

// WatchMetrics records usage every minute, on the minute.
func WatchMetrics(s *store.Store) {
	st := &metricState{}
	for {
		next := time.Now().Truncate(time.Minute).Add(time.Minute)
		time.Sleep(time.Until(next))
		recordUsage(s, st, next)
	}
}

func recordUsage(s *store.Store, st *metricState, now time.Time) {
	ts := now.Unix()
	for _, u := range collectUsage(s, st, now) {
		if err := putSample(s, minuteRes, ts, u); err != nil {
			panellog.Warn("failed to record the usage of", u.Target+":", err)
		}
	}
	// Summarize the five minutes before the current ones, once.
	if bucket := ts/fiveMinRes*fiveMinRes - fiveMinRes; bucket > st.rolled {
		if err := summarize(s, bucket); err != nil {
			panellog.Error("failed to summarize usage:", err)
		} else {
			st.rolled = bucket
		}
	}
	checkUsage(s, ts)
}

// putSample writes u into its slot of the ring of resolution res.
func putSample(s *store.Store, res, ts int64, u teldb.Sample) error {
	slots := int64(minuteSlots)
	if res == fiveMinRes {
		slots = fiveSlots
	}
	u.Res, u.Slot, u.Ts = res, ts/res%slots, ts
	return s.Tel.PutSample(ctx(), teldb.PutSampleParams(u))
}

// summarize writes the averages and peaks of the minutes in [bucket,
// bucket+5m) as one five-minute sample per target.
func summarize(s *store.Store, bucket int64) error {
	rows, err := s.Tel.SummarizeSamples(ctx(), teldb.SummarizeSamplesParams{Res: minuteRes, Ts: bucket, Ts_2: bucket + fiveMinRes})
	if err != nil {
		return err
	}
	for _, r := range rows {
		u := teldb.Sample{
			Target: r.Target, Cpu: r.Cpu, CpuMax: r.CpuMax, CpuLimit: r.CpuLimit,
			Mem: r.Mem, MemMax: r.MemMax, MemLimit: r.MemLimit,
			NetRx: r.NetRx, NetTx: r.NetTx, DiskRead: r.DiskRead, DiskWrite: r.DiskWrite,
			Load: r.Load, DiskUsed: r.DiskUsed, DiskTotal: r.DiskTotal,
		}
		if err := putSample(s, fiveMinRes, bucket, u); err != nil {
			return err
		}
	}
	return nil
}

// containerTarget is what a container's usage is recorded as.
type containerTarget struct {
	name     string
	cpuLimit float64
	memLimit int64
}

// containerTargets maps the containers on server sv worth recording by
// name: the live slot of each app there (not the candidate of a deploy),
// its worker, and hakobu's services. Targets don't change when a deploy
// swaps slots. A server is asked only about its own containers, and only
// they are taken from its answer: it could belong to someone else.
func containerTargets(s *store.Store, sv server) map[string]containerTarget {
	targets := map[string]containerTarget{
		node.PostgresContainer: {name: ServiceTarget("postgres", sv.Name)},
		node.TunnelContainer:   {name: ServiceTarget("cloudflared", sv.Name)},
		"buildkit":             {name: ServiceTarget("buildkit", sv.Name)},
	}
	for _, t := range tunnelsOn(s, sv) {
		if !t.acct.isPanel() {
			targets[t.acct.container()] = containerTarget{name: ServiceTarget("cloudflared-"+t.acct.Name, sv.Name)}
		}
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return targets
	}
	places, err := projectPlaces(s)
	if err != nil {
		return targets
	}
	for _, a := range apps {
		if places[a.ProjectID].server != sv.ID {
			continue
		}
		limit := containerTarget{cpuLimit: a.Cpus, memLimit: a.MemoryMB << 20}
		limit.name = "app:" + a.Name
		targets[a.ContainerName()] = limit
		limit.name = "worker:" + a.Name
		targets[a.Name+"-worker"] = limit
	}
	return targets
}

// collectUsage reads the usage of the last minute on every server
// connected. A container shows up from its second reading on, and not
// after it restarted, which resets its counters.
func collectUsage(s *store.Store, st *metricState, now time.Time) []teldb.Sample {
	var out []teldb.Sample
	for _, pn := range allNodes(s) {
		targets := containerTargets(s, pn.server)
		names := make(map[string]bool, len(targets))
		for name := range targets {
			names[name] = true
		}
		su := st.server(pn.Name)
		r, err := pn.n.Readings(ctx(), names)
		if r.HostErr == "" && r.Host.CPUTotal > 0 {
			if u, ok := hostUsage(su, r.Host, hostTarget(pn.Name)); ok {
				out = append(out, u)
			}
		}
		if err != nil {
			panellog.Warn("usage of", pn.label()+":", err)
			continue
		}
		seen := map[string]bool{}
		for _, c := range r.Containers {
			seen[c.ID] = true
			prev, had := su.containers[c.ID]
			su.containers[c.ID] = reading{c.Counters, now}
			if !had {
				continue
			}
			target, ok := targets[c.Name]
			if !ok {
				continue
			}
			if u, ok := containerUsage(target, prev, reading{c.Counters, now}); ok {
				out = append(out, u)
			}
		}
		for id := range su.containers {
			if !seen[id] {
				delete(su.containers, id)
			}
		}
	}
	return out
}

// containerUsage is the usage between two readings of a container.
func containerUsage(t containerTarget, prev, cur reading) (teldb.Sample, bool) {
	secs := cur.at.Sub(prev.at).Seconds()
	p, c := prev.c, cur.c
	if secs <= 0 || c.CPUNanos < p.CPUNanos || c.NetRx < p.NetRx || c.NetTx < p.NetTx || c.DiskRead < p.DiskRead || c.DiskWrite < p.DiskWrite {
		return teldb.Sample{}, false // restarted
	}
	rate := func(a, b uint64) float64 { return float64(b-a) / secs }
	cpu := float64(c.CPUNanos-p.CPUNanos) / 1e9 / secs
	mem := int64(c.Memory)
	return teldb.Sample{
		Target: t.name, Cpu: cpu, CpuMax: cpu, CpuLimit: t.cpuLimit,
		Mem: mem, MemMax: mem, MemLimit: t.memLimit,
		NetRx: rate(p.NetRx, c.NetRx), NetTx: rate(p.NetTx, c.NetTx),
		DiskRead: rate(p.DiskRead, c.DiskRead), DiskWrite: rate(p.DiskWrite, c.DiskWrite),
	}, true
}

// hostUsage is a server's usage since its last reading, recorded as
// target; its CPU shows from the second reading on.
func hostUsage(su *serverUsage, cur node.HostCounters, target string) (teldb.Sample, bool) {
	prev := su.host
	su.host = cur
	if prev.CPUTotal == 0 || cur.CPUTotal <= prev.CPUTotal || cur.CPUBusy < prev.CPUBusy {
		return teldb.Sample{}, false
	}
	u := teldb.Sample{Target: target, CpuLimit: float64(cur.CPUs)}
	u.Cpu = float64(cur.CPUBusy-prev.CPUBusy) / float64(cur.CPUTotal-prev.CPUTotal) * float64(cur.CPUs)
	u.CpuMax = u.Cpu
	if cur.MemTotal > 0 {
		u.Mem, u.MemLimit = cur.MemTotal-cur.MemFree, cur.MemTotal
		u.MemMax = u.Mem
	}
	u.Load = cur.Load
	u.DiskUsed, u.DiskTotal = int64(cur.DiskUsed), int64(cur.DiskTotal)
	return u, true
}

// Usage alerts: the last minutes, all of them over the threshold.
const (
	usageThreshold = 0.9
	memoryMinutes  = 10
	cpuMinutes     = 15
)

// checkUsage mails the owner about an app or worker near its memory limit,
// and a server short of memory or CPU, for minutes on end.
func checkUsage(s *store.Store, ts int64) {
	memHigh := func(r teldb.Sample) bool {
		return r.MemLimit > 0 && float64(r.Mem) >= usageThreshold*float64(r.MemLimit)
	}
	cpuHigh := func(r teldb.Sample) bool {
		return r.CpuLimit > 0 && r.Cpu >= usageThreshold*r.CpuLimit
	}
	if apps, err := s.ListApps(ctx()); err == nil {
		for _, a := range apps {
			if a.MemoryMB == 0 {
				continue
			}
			for _, target := range []string{"app:" + a.Name, "worker:" + a.Name} {
				what := a.Name
				if strings.HasPrefix(target, "worker:") {
					what += "'s worker"
				}
				alertOn(s, target, ts, memoryMinutes, memHigh, "memory:"+target,
					what+": close to its memory limit",
					fmt.Sprintf("%s has used over %d%% of its %d MB memory limit for %d minutes; at the limit it's killed. Raise the limit or find what grows: %s",
						what, int(usageThreshold*100), a.MemoryMB, memoryMinutes, panelURL("/apps/"+a.Name)),
					what+": memory back under its limit", what+" uses less than "+strconv.Itoa(int(usageThreshold*100))+"% of its memory limit again.")
			}
		}
	}
	alertOn(s, HostTarget, ts, memoryMinutes, memHigh, "host-memory",
		"The server is short of memory",
		fmt.Sprintf("Over %d%% of the server's memory has been in use for %d minutes; apps and databases get killed when it runs out. See what uses it: %s",
			int(usageThreshold*100), memoryMinutes, panelURL("/settings#server")),
		"The server has memory to spare again", "Memory use of the server is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
	alertOn(s, HostTarget, ts, cpuMinutes, cpuHigh, "host-cpu",
		"The server's CPU is maxed out",
		fmt.Sprintf("The server's CPUs have been over %d%% busy for %d minutes; apps answer slowly. See what uses them: %s",
			int(usageThreshold*100), cpuMinutes, panelURL("/settings#server")),
		"The server's CPU has room again", "CPU use of the server is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
}

// alertOn mails problem when every minute of the last ones of target is
// high, and solved once the latest isn't.
func alertOn(s *store.Store, target string, ts int64, minutes int, high func(teldb.Sample) bool, key, subject, text, okSubject, okText string) {
	rows, err := s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: target, Res: minuteRes, Ts: ts - int64(minutes)*60 + 1})
	if err != nil || len(rows) == 0 {
		return
	}
	all := len(rows) >= minutes-1 // a minute may be missed
	for _, r := range rows {
		all = all && high(r)
	}
	switch {
	case all:
		problem(s, key, notifyAgain, subject, text)
	case !high(rows[len(rows)-1]):
		solved(s, key, okSubject, okText)
	}
}

// UsageOf reads the usage of target over the last span: by the minute up
// to 48 hours, by five minutes beyond.
func UsageOf(s *store.Store, target string, span time.Duration) ([]teldb.Sample, error) {
	res := int64(minuteRes)
	if span > minuteSlots*time.Minute {
		res = fiveMinRes
	}
	return s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: target, Res: res, Ts: time.Now().Add(-span).Unix()})
}

// CurrentUsage is the latest usage of what runs on the panel's server, as
// user sees it: the server first, then by name, with other users' apps
// summed up as Target "others".
func CurrentUsage(s *store.Store, user int64) ([]teldb.Sample, error) {
	rows, err := s.Tel.LatestSamples(ctx(), teldb.LatestSamplesParams{Res: minuteRes, Ts: time.Now().Add(-3 * time.Minute).Unix()})
	if err != nil {
		return nil, err
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return nil, err
	}
	projects, err := s.ListProjects(ctx())
	if err != nil {
		return nil, err
	}
	type where struct {
		local, mine bool
	}
	byProject := map[int64]where{}
	for _, p := range projects {
		byProject[p.ID] = where{local: !p.NodeID.Valid, mine: p.UserID == user}
	}
	appOf := map[string]where{}
	for _, a := range apps {
		appOf[a.Name] = byProject[a.ProjectID]
	}
	latest := map[string]teldb.Sample{}
	for _, r := range rows { // oldest first
		kind, name, _ := strings.Cut(r.Target, ":")
		switch kind {
		case HostTarget, "service":
			if strings.Contains(name, ":") || (kind == HostTarget && name != "") {
				continue // another server's
			}
		case "app", "worker":
			w, ok := appOf[name]
			if !ok || !w.local {
				continue
			}
			if !w.mine {
				r.Target = "others:" + r.Target
			}
		default:
			continue
		}
		latest[r.Target] = r
	}
	out := make([]teldb.Sample, 0, len(latest))
	others := teldb.Sample{Target: "others"}
	for _, r := range latest {
		if strings.HasPrefix(r.Target, "others:") {
			others.Cpu += r.Cpu
			others.Mem += r.Mem
			continue
		}
		out = append(out, r)
	}
	if others.Cpu > 0 || others.Mem > 0 {
		out = append(out, others)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Target == HostTarget) != (out[j].Target == HostTarget) {
			return out[i].Target == HostTarget
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}
