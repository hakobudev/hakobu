package node

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Host is the machine at a glance, for status: what it runs, how long it's
// been up, and how full it is.
type Host struct {
	OS        string  `json:"os,omitempty"` // "Debian 13"
	Arch      string  `json:"arch"`
	Uptime    int64   `json:"uptime,omitempty"` // seconds
	DiskUsed  uint64  `json:"disk_used,omitempty"`
	DiskTotal uint64  `json:"disk_total,omitempty"`
	MemUsed   int64   `json:"mem_used,omitempty"`
	MemTotal  int64   `json:"mem_total,omitempty"`
	CPU       float64 `json:"cpu"` // busy share of all cores over a moment, 0–1
}

// osRelease is where the distribution says what it is; tests point it at
// fixtures.
var osRelease = "/etc/os-release"

// Host reads the machine; what can't be read is left out.
func (l Local) Host(ctx context.Context) Host {
	h := Host{Arch: runtime.GOARCH, OS: readOS()}
	if b, err := os.ReadFile(filepath.Join(procRoot, "uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if s, err := strconv.ParseFloat(f[0], 64); err == nil {
				h.Uptime = int64(s)
			}
		}
	}
	if used, total, err := l.Disk(ctx); err == nil {
		h.DiskUsed, h.DiskTotal = used, total
	}
	if total, avail, err := readMemInfo(); err == nil {
		h.MemUsed, h.MemTotal = total-avail, total
	}
	h.CPU = cpuBusy(ctx, 250*time.Millisecond)
	return h
}

// readOS is "Debian 13" from ID and VERSION_ID.
func readOS() string {
	b, err := os.ReadFile(osRelease)
	if err != nil {
		return ""
	}
	kv := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	id := kv["ID"]
	if id == "" {
		return ""
	}
	return strings.TrimSpace(strings.ToUpper(id[:1]) + id[1:] + " " + kv["VERSION_ID"])
}

// cpuBusy is the share of time all cores were busy over d, from /proc/stat.
func cpuBusy(ctx context.Context, d time.Duration) float64 {
	busy0, all0, ok := cpuTimes()
	if !ok {
		return 0
	}
	select {
	case <-ctx.Done():
		return 0
	case <-time.After(d):
	}
	busy1, all1, ok := cpuTimes()
	if !ok || all1 <= all0 {
		return 0
	}
	return float64(busy1-busy0) / float64(all1-all0)
}

// cpuTimes are the busy and all jiffies on the "cpu" line of /proc/stat:
// idle and iowait aren't busy.
func cpuTimes() (busy, all uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, s := range f[1:] {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		all += n
		if i != 3 && i != 4 { // idle, iowait
			busy += n
		}
	}
	return busy, all, true
}
