package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDockerReserve keeps a reserve next to the working directory, as on a
// server: HAKOBU_DOCKER_TEST=1 go test ./internal/node -run DockerReserve
func TestDockerReserve(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	dir, err := os.MkdirTemp(".", "reserve-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "disk-reserve")
	err = keepReserve(t.Context(), path)
	if errors.Is(err, errOtherDisk) {
		t.Skip("this checkout isn't on Docker's disk")
	}
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("no reserve: %v", err)
	} else {
		t.Logf("kept %d MB", fi.Size()>>20)
	}
}
