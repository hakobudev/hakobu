package cmd

import (
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/store/teldb"
)

func TestUsageChart(t *testing.T) {
	rows := []teldb.Sample{
		{Ts: 0, Mem: 100 << 20, MemMax: 150 << 20, MemLimit: 512 << 20},
		{Ts: 60, Mem: 200 << 20, MemMax: 200 << 20, MemLimit: 512 << 20},
		{Ts: 600, Mem: 300 << 20, MemMax: 400 << 20, MemLimit: 512 << 20}, // after a gap
	}
	charts := targetCharts("app:web", rows, 0, 600)
	if len(charts) != 3 {
		t.Fatalf("%d charts, want CPU, memory and network", len(charts))
	}
	mem := charts[1]
	if mem.Title != "App · memory" || mem.Now != "300 MB of 512 MB" || mem.Peak != "peak 400 MB" {
		t.Errorf("memory chart reads %q, %q, %q", mem.Title, mem.Now, mem.Peak)
	}
	svg := string(mem.SVG)
	if n := strings.Count(svg, "<polyline"); n != 2 {
		t.Errorf("%d lines, want the gap to split the series in 2", n)
	}
	if !strings.Contains(svg, `stroke-dasharray`) {
		t.Error("no line for the memory limit")
	}
	if mem.YTicks[2] == "0 B" {
		t.Error("memory's scale starts at 0, flattening it")
	}
	if !strings.Contains(mem.Points, `"t":[0,60,600]`) || !strings.Contains(mem.Points, `"300 MB (peak 400 MB)"`) {
		t.Errorf("the pointer reads %s", mem.Points)
	}
	if got := usageRows(rows[:1], nil)[0]; got.MemPct != 20 {
		t.Errorf("memory at %d%% of the limit, want 20", got.MemPct)
	}
}

// Two spikes in a quiet day don't flatten it: the scale tops out near
// the rest, and the spikes are cut and marked.
func TestUsageChartClipsSpikes(t *testing.T) {
	var rows []teldb.Sample
	for i := range 1440 {
		cpu := 0.03
		if i == 1300 || i == 1350 {
			cpu = 0.87
		}
		rows = append(rows, teldb.Sample{Ts: int64(i) * 60, Cpu: cpu, CpuMax: cpu})
	}
	c := targetCharts("app:1", rows, 0, 1440*60)[0]
	if c.YTicks[0] != "0.04 CPU" || len(c.Clipped) != 2 || c.Peak != "peak 0.87 CPU" {
		t.Errorf("scale %v, clipped at %v, %s", c.YTicks, c.Clipped, c.Peak)
	}
	if strings.Contains(string(c.SVG), ",-") {
		t.Error("a line goes over the top")
	}
}

func TestSummarizeUsageForMCP(t *testing.T) {
	var rows []teldb.Sample
	for i := range 120 {
		rows = append(rows, teldb.Sample{Ts: int64(i) * 60, Cpu: 0.5, CpuMax: 0.5, CpuLimit: 1, Mem: 100 << 20, MemMax: 100 << 20})
	}
	rows[7].CpuMax, rows[7].MemMax = 0.9, 300<<20
	u := summarizeUsage("app:web", "24h", rows)
	if len(u.Series) != 60 {
		t.Errorf("%d points, want 60", len(u.Series))
	}
	if u.CPU != (mcpStat{Now: 0.5, Avg: 0.5, Peak: 0.9, Limit: 1}) || u.Memory.Peak != 300 || u.Memory.Limit != 0 {
		t.Errorf("stats %+v %+v", u.CPU, u.Memory)
	}
	if u.Series[3].CPUPeak != 0.9 {
		t.Errorf("the peak got lost thinning the series: %+v", u.Series[3])
	}
	if empty := summarizeUsage("host", "1h", nil); empty.Series == nil {
		t.Error("no samples should still be an empty list, not null")
	}
}

// A server's disk: where it is and how fast it grows, over the range and
// the last hour.
func TestSummarizeDiskForMCP(t *testing.T) {
	var rows []teldb.Sample
	for i := range 120 { // two hours, half a GB a minute in the second
		used := int64(8 << 30)
		if i >= 60 {
			used += int64(i-59) << 29
		}
		rows = append(rows, teldb.Sample{Ts: int64(i) * 60, DiskUsed: used, DiskTotal: 40 << 30})
	}
	u := summarizeUsage("host:3", "24h", rows)
	if d := u.Disk; d == nil || d.UsedGB != 38 || d.TotalGB != 40 || d.UsedPercent != 95 || d.GrownGB != 30 || d.LastHourGB != 30 {
		t.Errorf("disk %+v", u.Disk)
	}
	if u.Series[len(u.Series)-1].DiskGB != 38 {
		t.Errorf("last point %+v", u.Series[len(u.Series)-1])
	}
	if summarizeUsage("app:web", "24h", []teldb.Sample{{Ts: 1, Mem: 1}}).Disk != nil {
		t.Error("an app has no disk")
	}
}

// A service's charts are titled the same on any server: the server's ID in
// its target isn't for people.
func TestTargetTitle(t *testing.T) {
	for target, want := range map[string]string{
		"service:postgres":      "PostgreSQL, all databases",
		"service:postgres:3":    "PostgreSQL, all databases",
		"service:cloudflared:3": "cloudflared",
		"host:3":                "Server",
	} {
		if got := targetTitle(target); got != want {
			t.Errorf("targetTitle(%q) = %q, want %q", target, got, want)
		}
	}
}

// get_app_logs says when the last line was written, and when Docker
// couldn't read the log to its end.
func TestLogsEnd(t *testing.T) {
	logs := "2026-10-16T15:43:59.123456789Z GET /health 200\n2026-10-16T15:44:01.5Z POST /runs 500\n"
	if at, note := logsEnd(logs); at != "2026-10-16T15:44:01Z" || note != "" {
		t.Errorf("%q %q", at, note)
	}
	cut := logs + "Error grabbing logs: invalid character '\\x00' looking for beginning of value\n"
	if at, note := logsEnd(cut); at != "2026-10-16T15:44:01Z" || !strings.Contains(note, "damaged") {
		t.Errorf("damaged: %q %q", at, note)
	}
	if at, note := logsEnd(""); at != "" || note != "" {
		t.Errorf("empty: %q %q", at, note)
	}
}
