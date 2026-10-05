package build

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The app's RAILPACK_* variables reach railpack by name on its command
// line, their values only in its environment: a command line can be read
// by anyone on the server.
func TestRailpackGetsTheAppsVariables(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	got := filepath.Join(dir, "got")
	script := "#!/bin/sh\necho \"$*\" > " + got + "\necho \"$RAILPACK_START_CMD\" >> " + got + "\n"
	if err := os.WriteFile(filepath.Join(bin, "railpack"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILDKIT_HOST", "tcp://unused") // no buildkit container

	env := []string{"RAILPACK_START_CMD=node shared.js", "RAILPACK_START_CMD=node app.js", "RAILPACK_NODE_VERSION=22"}
	if err := BuildWithStrategy(dir, "img", "railpack", env, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if want := "build " + dir + " --name img --env RAILPACK_START_CMD --env RAILPACK_NODE_VERSION"; lines[0] != want {
		t.Errorf("railpack %s, want %s", lines[0], want)
	}
	if lines[1] != "node app.js" {
		t.Errorf("RAILPACK_START_CMD = %q, want the app's own over the shared one", lines[1])
	}
}
