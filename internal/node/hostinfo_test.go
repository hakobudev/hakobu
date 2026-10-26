package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadOS(t *testing.T) {
	for in, want := range map[string]string{
		"PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\nVERSION_ID=\"13\"\n": "Debian 13",
		"NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n":                           "Ubuntu 24.04",
		"NAME=whatever\n": "",
	} {
		f := filepath.Join(t.TempDir(), "os-release")
		if err := os.WriteFile(f, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
		old := osRelease
		osRelease = f
		if got := readOS(); got != want {
			t.Errorf("readOS(%q) = %q, want %q", in, got, want)
		}
		osRelease = old
	}
}

func TestCPUTimes(t *testing.T) {
	dir := t.TempDir()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
	// user nice system idle iowait irq softirq steal
	stat := "cpu  100 0 50 800 50 0 0 0\ncpu0 100 0 50 800 50 0 0 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	busy, all, ok := cpuTimes()
	if !ok || busy != 150 || all != 1000 {
		t.Fatalf("cpuTimes = %d, %d, %v; want 150, 1000, true", busy, all, ok)
	}
}
