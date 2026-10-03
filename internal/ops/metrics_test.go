package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

func TestHostUsage(t *testing.T) {
	st := &serverUsage{}
	read := node.HostCounters{CPUBusy: 200, CPUTotal: 1000, CPUs: 2, MemTotal: 4000 << 10, MemFree: 1000 << 10, Load: 0.42}
	if _, ok := hostUsage(st, read, HostTarget); ok {
		t.Error("CPU usage from a single reading")
	}
	// 300 more ticks, 150 of them busy: half of 2 CPUs.
	read.CPUBusy, read.CPUTotal = 350, 1300
	u, ok := hostUsage(st, read, HostTarget)
	if !ok {
		t.Fatal("no usage from two readings")
	}
	if u.Cpu != 1 || u.CpuLimit != 2 {
		t.Errorf("CPU %v of %v cores, want 1 of 2", u.Cpu, u.CpuLimit)
	}
	if u.Mem != 3000<<10 || u.MemLimit != 4000<<10 || u.Load != 0.42 {
		t.Errorf("memory %d of %d, load %v", u.Mem, u.MemLimit, u.Load)
	}
}

func TestContainerUsage(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	prev := reading{node.Counters{CPUNanos: 1e9, Memory: 100, NetRx: 1000, NetTx: 0}, at}
	cur := reading{node.Counters{CPUNanos: 31e9, Memory: 200, NetRx: 7000, NetTx: 600}, at.Add(time.Minute)}
	u, ok := containerUsage(containerTarget{name: "app:web", cpuLimit: 1, memLimit: 512 << 20}, prev, cur)
	if !ok {
		t.Fatal("no usage")
	}
	if u.Cpu != 0.5 || u.Mem != 200 || u.NetRx != 100 || u.NetTx != 10 || u.CpuLimit != 1 || u.MemLimit != 512<<20 {
		t.Errorf("usage %+v", u)
	}
	if _, ok := containerUsage(containerTarget{name: "app:web"}, cur, prev); ok {
		t.Error("usage across a restart, whose counters start over")
	}
}

// The rings keep their size: a slot is written over a full turn later, and
// readers only see the samples of the span asked for.
func TestUsageRings(t *testing.T) {
	s := notifyStore(t)
	now := time.Now().Truncate(time.Minute).Unix()
	put := func(ts int64, cpu float64) {
		if err := putSample(s, minuteRes, ts, teldb.Sample{Target: "app:web", Cpu: cpu, CpuMax: cpu, Mem: int64(cpu * 100), MemMax: int64(cpu * 100)}); err != nil {
			t.Fatal(err)
		}
	}
	put(now-minuteSlots*60, 9) // a full turn ago: the same slot
	put(now, 1)
	rows, err := UsageOf(s, "app:web", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Cpu != 1 {
		t.Errorf("rows %+v, want the newest only", rows)
	}

	bucket := now / fiveMinRes * fiveMinRes
	for i, cpu := range []float64{1, 2, 3, 4, 5} {
		put(bucket+int64(i)*60, cpu)
	}
	if err := summarize(s, bucket); err != nil {
		t.Fatal(err)
	}
	if err := summarize(s, bucket); err != nil { // after a restart: same result
		t.Fatal(err)
	}
	rows, err = s.Tel.ListSamples(ctx(), teldb.ListSamplesParams{Target: "app:web", Res: fiveMinRes, Ts: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Cpu != 3 || rows[0].CpuMax != 5 || rows[0].MemMax != 500 {
		t.Errorf("five minutes summarized as %+v, want avg 3, peak 5", rows)
	}
}

func TestUsageAlerts(t *testing.T) {
	f := newFakeEmail(t)
	s := notifyStore(t)
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute).Unix()
	host := func(from int64, minutes int, used int64) {
		for i := range minutes {
			if err := putSample(s, minuteRes, from+int64(i)*60, teldb.Sample{Target: HostTarget, Mem: used, MemLimit: 100, CpuLimit: 4, Cpu: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	host(now-20*60, 15, 95) // 15 minutes, but the last 5 are missing
	checkUsage(s, now)
	host(now-9*60, 10, 95)
	checkUsage(s, now)
	host(now, 1, 50)
	checkUsage(s, now)

	if got, want := f.sent(), []string{"The server is short of memory", "The server has memory to spare again"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sent %q, want %q", got, want)
	}
}
