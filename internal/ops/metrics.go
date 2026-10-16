package ops

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
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
	servers map[int64]*serverUsage // by server ID, 0 for the panel's
	rolled  int64                  // the last five minutes summarized
}

// serverUsage is a server's last readings.
type serverUsage struct {
	containers map[string]reading // by container ID
	host       node.HostCounters
}

func (st *metricState) server(id int64) *serverUsage {
	if st.servers == nil {
		st.servers = map[int64]*serverUsage{}
	}
	if st.servers[id] == nil {
		st.servers[id] = &serverUsage{containers: map[string]reading{}}
	}
	return st.servers[id]
}

// hostTarget is what a server's own usage is recorded as: by its ID, as
// names are only unique among a user's servers.
func hostTarget(serverID int64) string {
	if serverID == 0 {
		return HostTarget
	}
	return fmt.Sprintf("%s:%d", HostTarget, serverID)
}

// ServiceTarget is what hakobu's service name (postgres, cloudflared,
// buildkit) on a server (0: the panel's) is recorded as.
func ServiceTarget(name string, serverID int64) string {
	if serverID == 0 {
		return "service:" + name
	}
	return fmt.Sprintf("service:%s:%d", name, serverID)
}

// AppTarget is what an app's usage is recorded as, WorkerTarget its
// worker's.
func AppTarget(id int64) string    { return fmt.Sprintf("app:%d", id) }
func WorkerTarget(id int64) string { return fmt.Sprintf("worker:%d", id) }

// CacheTarget is what a cache's usage is recorded as.
func CacheTarget(id int64) string { return fmt.Sprintf("cache:%d", id) }

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
// its worker, each cache, and hakobu's services. Targets don't change when a deploy
// swaps slots. A server is asked only about its own containers, and only
// they are taken from its answer: it could belong to someone else.
func containerTargets(s *store.Store, sv server) map[string]containerTarget {
	targets := map[string]containerTarget{
		node.PostgresContainer: {name: ServiceTarget("postgres", sv.ID)},
		node.TunnelContainer:   {name: ServiceTarget("cloudflared", sv.ID)},
		"buildkit":             {name: ServiceTarget("buildkit", sv.ID)},
	}
	for _, t := range tunnelsOn(s, sv) {
		if !t.acct.isPanel() {
			targets[t.acct.container()] = containerTarget{name: ServiceTarget("cloudflared-"+t.acct.Name, sv.ID)}
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
		limit.name = AppTarget(a.ID)
		targets[a.ContainerName()] = limit
		limit.name = WorkerTarget(a.ID)
		targets[node.WorkerContainer(a.Key())] = limit
	}
	caches, err := s.ListCaches(ctx())
	if err != nil {
		return targets
	}
	for _, c := range caches {
		if places[c.ProjectID].server == sv.ID {
			targets[c.Key()] = containerTarget{name: CacheTarget(c.ID), memLimit: c.MemoryMB << 20}
		}
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
		su := st.server(pn.ID)
		r, err := pn.n.Readings(ctx(), names)
		if r.HostErr == "" && r.Host.CPUTotal > 0 {
			if u, ok := hostUsage(su, r.Host, hostTarget(pn.ID)); ok {
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
			for _, target := range []string{AppTarget(a.ID), WorkerTarget(a.ID)} {
				what := a.Name
				if strings.HasPrefix(target, "worker:") {
					what += "'s worker"
				}
				alertOn(s, target, ts, memoryMinutes, memHigh, "memory:"+target,
					what+": close to its memory limit",
					fmt.Sprintf("%s has used over %d%% of its %d MB memory limit for %d minutes; at the limit it's killed. Raise the limit or find what grows: %s",
						what, int(usageThreshold*100), a.MemoryMB, memoryMinutes, panelURL(AppPath(a))),
					what+": memory back under its limit", what+" uses less than "+strconv.Itoa(int(usageThreshold*100))+"% of its memory limit again.")
			}
		}
	}
	for _, h := range alertHosts(s) {
		alertOn(s, h.target, ts, memoryMinutes, memHigh, h.key("memory"),
			h.label+" is short of memory",
			fmt.Sprintf("Over %d%% of %s's memory has been in use for %d minutes; apps and databases get killed when it runs out. See what uses it: %s",
				int(usageThreshold*100), h.what, memoryMinutes, h.page),
			h.label+" has memory to spare again", "Memory use of "+h.what+" is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
		alertOn(s, h.target, ts, cpuMinutes, cpuHigh, h.key("cpu"),
			h.label+": CPU maxed out",
			fmt.Sprintf("The CPUs of %s have been over %d%% busy for %d minutes; apps answer slowly. See what uses them: %s",
				h.what, int(usageThreshold*100), cpuMinutes, h.page),
			h.label+": CPU has room again", "CPU use of "+h.what+" is back under "+strconv.Itoa(int(usageThreshold*100))+"%.")
		checkDisk(s, h, ts)
	}
}

// ServerDisk is the server's (0: the panel's) disk at its latest reading,
// zeros when there's none from the last few minutes.
func ServerDisk(s *store.Store, serverID int64) (used, total int64) {
	rows, err := s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: hostTarget(serverID), Res: minuteRes, Ts: time.Now().Add(-3 * time.Minute).Unix()})
	if err != nil || len(rows) == 0 {
		return 0, 0
	}
	r := rows[len(rows)-1]
	return r.DiskUsed, r.DiskTotal
}

// alertHost is a server as its usage alerts name it.
type alertHost struct {
	id     int64  // 0: the panel's
	target string // its usage's
	label  string // "The server" or "Server kasl", to start a subject
	what   string // "the server" or "server kasl", in a sentence
	page   string // where to look
}

// key is the alert's key: the panel's server's own (host-memory, disk...)
// or another's by its ID (servermemory:<id>, see alertAbout).
func (h alertHost) key(kind string) string {
	if h.id == 0 {
		if kind == "disk" {
			return "disk" // the key it had when only the panel's was watched
		}
		return "host-" + kind
	}
	return fmt.Sprintf("server%s:%d", kind, h.id)
}

// alertHosts are the panel's server and every joined one.
func alertHosts(s *store.Store) []alertHost {
	out := []alertHost{{target: HostTarget, label: "The server", what: "the server", page: panelURL("/settings#server")}}
	nodes, _ := s.ListNodes(ctx())
	for _, n := range nodes {
		out = append(out, alertHost{id: n.ID, target: hostTarget(n.ID), label: "Server " + n.Name, what: "server " + n.Name, page: panelURL("/settings#servers")})
	}
	return out
}

// Disk alerts: a server's disk this full, or filling so fast it'd be full
// within diskSoon at the pace of up to the last diskPaceMinutes (at least
// diskPaceFrom of them, and diskPaceMin grown: a build pulls a GB or so).
const (
	diskFilling     = 0.8
	diskFull        = 0.9
	diskSoon        = 2 * time.Hour
	diskPaceMinutes = 30
	diskPaceFrom    = 10
	diskPaceMin     = 1 << 30
	diskFastAgain   = time.Hour
)

// checkDisk mails the owner when the server's disk passes diskFilling and
// diskFull, and when it's on course to fill within diskSoon: a database
// or a log growing out of hand fills one in under an hour.
func checkDisk(s *store.Store, h alertHost, ts int64) {
	rows, err := s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: h.target, Res: minuteRes, Ts: ts - diskPaceMinutes*60})
	if err != nil || len(rows) == 0 {
		return
	}
	cur := rows[len(rows)-1]
	if cur.DiskTotal == 0 || cur.Ts < ts-3*60 { // not read lately: its server is away
		return
	}
	used, total := cur.DiskUsed, cur.DiskTotal
	pct := float64(used) / float64(total)
	state := fmt.Sprintf("%s of %s used (%.0f%%)", humanBytes(uint64(used)), humanBytes(uint64(total)), pct*100)
	advice := "Deploys, databases and logs fail on a full disk. See what grows (a database's size is on its page) and free some space: " + h.page
	for _, level := range []struct {
		at   float64
		kind string
		subj string
	}{{diskFull, "disk", " is almost full"}, {diskFilling, "diskfilling", " is filling up"}} {
		if pct >= level.at {
			problem(s, h.key(level.kind), notifyAgain, h.label+"'s disk"+level.subj, "Disk: "+state+". "+advice)
		} else {
			solved(s, h.key(level.kind), h.label+"'s disk has room again", "Disk: "+state+".")
		}
	}
	first := rows[0]
	grown, span := used-first.DiskUsed, cur.Ts-first.Ts
	if span >= diskPaceFrom*60 && grown >= diskPaceMin {
		perSec := float64(grown) / float64(span)
		left := time.Duration(float64(total-used)/perSec) * time.Second
		if left < diskSoon {
			problem(s, h.key("diskfast"), diskFastAgain, h.label+"'s disk is filling fast",
				fmt.Sprintf("Disk: %s, up %s in the last %d minutes: at this pace it's full in about %d minutes. %s",
					state, humanBytes(uint64(grown)), span/60, int(left.Minutes())+1, advice))
			return
		}
	}
	if grown < diskPaceMin/4 {
		solved(s, h.key("diskfast"), h.label+"'s disk stopped filling", "Disk: "+state+".")
	}
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
	appOf := map[string]where{} // by ID
	for _, a := range apps {
		appOf[strconv.FormatInt(a.ID, 10)] = byProject[a.ProjectID]
	}
	caches, err := s.ListCaches(ctx())
	if err != nil {
		return nil, err
	}
	cacheOf := map[string]where{} // by ID
	for _, c := range caches {
		cacheOf[strconv.FormatInt(c.ID, 10)] = byProject[c.ProjectID]
	}
	latest := map[string]teldb.Sample{}
	for _, r := range rows { // oldest first
		kind, name, _ := strings.Cut(r.Target, ":")
		switch kind {
		case HostTarget, "service":
			if strings.Contains(name, ":") || (kind == HostTarget && name != "") {
				continue // another server's
			}
		case "app", "worker", "cache":
			w, ok := appOf[name]
			if kind == "cache" {
				w, ok = cacheOf[name]
			}
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
