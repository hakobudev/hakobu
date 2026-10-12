package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/store"
)

// A command's import goes to the running panel and comes back with its
// answer; one that finds no panel says so.
func TestCLISocketImport(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := os.Mkdir("data", 0o700); err != nil {
		t.Fatal(err)
	}

	if err := panelImport(1, strings.NewReader("PGDMP")); err == nil || !strings.Contains(err.Error(), "is hakobu running") {
		t.Fatalf("without a panel: %v", err)
	}

	ln, err := listenCLISocket()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if fi, err := os.Stat(cliSocket); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket %v, %v", fi.Mode(), err)
	}
	go serveCLI(s, ln)

	if err := panelImport(42, strings.NewReader("PGDMP")); err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("unknown database: %v", err)
	}
}
