package node

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestPostgresShm(t *testing.T) {
	dir := t.TempDir()
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })
	for _, c := range []struct{ memKB, wantMB int64 }{
		{16 << 20, 1024},  // 16 GB: the 1 GB cap
		{4 << 20, 1024},   // 4 GB: a quarter is the cap
		{2 << 20, 512},    // 2 GB: a quarter
		{1900 << 10, 448}, // 1.9 GB: a quarter, down to 64 MB steps
		{200 << 10, 64},   // 200 MB: never below Docker's 64 MB
	} {
		body := fmt.Sprintf("MemTotal: %d kB\nMemAvailable: 1 kB\n", c.memKB)
		if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := postgresShm(); err != nil || got != c.wantMB<<20 {
			t.Errorf("%d kB of RAM: %d MB (%v), want %d MB", c.memKB, got>>20, err, c.wantMB)
		}
	}
}
