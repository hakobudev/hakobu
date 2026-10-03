package ops

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
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
	containers map[string]reading // by container ID
	host       node.HostCounters
	rolled     int64 // the last five minutes summarized
}

// WatchMetrics records usage every minute, on the minute.
func WatchMetrics(s *store.Store) {
	st := &metricState{containers: map[string]reading{}}
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
			fmt.Println("failed to record the usage of", u.Target+":", err)
		}
	}
	// Summarize the five minutes before the current ones, once.
	if bucket := ts/fiveMinRes*fiveMinRes - fiveMinRes; bucket > st.rolled {
		if err := summarize(s, bucket); err != nil {
			fmt.Println("failed to summarize usage:", err)
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

// containerTargets maps the containers worth recording by name: the live
// slot of each app (not the candidate of a deploy), its worker, and
// hakobu's services. Targets don't change when a deploy swaps slots.
func containerTargets(s *store.Store) map[string]containerTarget {
	targets := map[string]containerTarget{
		node.PostgresContainer: {name: "service:postgres"},
		node.TunnelContainer:   {name: "service:cloudflared"},
		"buildkit":             {name: "service:buildkit"},
	}
	for _, a := range tunnelAccounts(s) {
		if !a.isPanel() {
			targets[a.container()] = containerTarget{name: "service:cloudflared-" + a.Name}
		}
	}
	apps, err := s.ListApps(ctx())
	if err != nil {
		return targets
	}
	for _, a := range apps {
		limit := containerTarget{cpuLimit: a.Cpus, memLimit: a.MemoryMB << 20}
		limit.name = "app:" + a.Name
		targets[a.ContainerName()] = limit
		limit.name = "worker:" + a.Name
		targets[a.Name+"-worker"] = limit
	}
	return targets
}

// collectUsage reads the usage of the last minute. A container shows up
// from its second reading on, and not after it restarted, which resets
// its counters.
//
// The server is the panel's node; other nodes' usage comes with them.
func collectUsage(s *store.Store, st *metricState, now time.Time) []teldb.Sample {
	targets := containerTargets(s)
	names := make(map[string]bool, len(targets))
	for name := range targets {
		names[name] = true
	}
	r, err := local.Readings(ctx(), names)
	var out []teldb.Sample
	if r.HostErr == nil {
		if u, ok := hostUsage(st, r.Host); ok {
			out = append(out, u)
		}
	}
	if err != nil {
		fmt.Println("usage:", err)
		return out
	}
	seen := map[string]bool{}
	for _, c := range r.Containers {
		seen[c.ID] = true
		prev, had := st.containers[c.ID]
		st.containers[c.ID] = reading{c.Counters, now}
		if !had {
			continue
		}
		if u, ok := containerUsage(targets[c.Name], prev, reading{c.Counters, now}); ok {
			out = append(out, u)
		}
	}
	for id := range st.containers {
		if !seen[id] {
			delete(st.containers, id)
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

// hostUsage is the server's usage since the last reading; its CPU shows
// from the second reading on.
func hostUsage(st *metricState, cur node.HostCounters) (teldb.Sample, bool) {
	prev := st.host
	st.host = cur
	if prev.CPUTotal == 0 || cur.CPUTotal <= prev.CPUTotal || cur.CPUBusy < prev.CPUBusy {
		return teldb.Sample{}, false
	}
	u := teldb.Sample{Target: HostTarget, CpuLimit: float64(cur.CPUs)}
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

// CurrentUsage is the latest usage of every target, the server first,
// then by name.
func CurrentUsage(s *store.Store) ([]teldb.Sample, error) {
	rows, err := s.Tel.LatestSamples(ctx(), teldb.LatestSamplesParams{Res: minuteRes, Ts: time.Now().Add(-3 * time.Minute).Unix()})
	if err != nil {
		return nil, err
	}
	latest := map[string]teldb.Sample{}
	for _, r := range rows { // oldest first
		latest[r.Target] = r
	}
	out := make([]teldb.Sample, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Target == HostTarget) != (out[j].Target == HostTarget) {
			return out[i].Target == HostTarget
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}
