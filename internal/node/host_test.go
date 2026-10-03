package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostCountersFromProc(t *testing.T) {
	dir := t.TempDir()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// user nice system idle iowait irq softirq steal guest guest_nice
	write("stat", "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 0 0 0 0 0 0 0 0 0\ncpu1 1 0 0 0 0 0 0 0 0 0\nintr 1\n")
	write("meminfo", "MemTotal:       4000 kB\nMemFree:         500 kB\nMemAvailable:   1000 kB\n")
	write("loadavg", "0.42 0.30 0.20 1/100 1234\n")
	h, err := readHost(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Guest time is in user already; idle and iowait aren't busy.
	if h.CPUBusy != 200 || h.CPUTotal != 1000 || h.CPUs != 2 {
		t.Errorf("CPU %d busy of %d on %d CPUs", h.CPUBusy, h.CPUTotal, h.CPUs)
	}
	if h.MemTotal != 4000<<10 || h.MemFree != 1000<<10 || h.Load != 0.42 {
		t.Errorf("memory %d, available %d, load %v", h.MemTotal, h.MemFree, h.Load)
	}
}
