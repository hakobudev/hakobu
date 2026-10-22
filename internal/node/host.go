package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hakobudev/hakobu/internal/deploy"
)

// The machine: its memory and disk, and its apps' health.

// Disk is the use of the disk Docker keeps its data on.
func (Local) Disk(ctx context.Context) (used, total uint64, err error) {
	return deploy.Disk(ctx)
}

// procRoot is where the kernel's /proc is; tests point it at fixtures.
var procRoot = "/proc"

// readMemInfo reads the machine's memory and how much of it is available,
// in bytes.
func readMemInfo() (total, available int64, err error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			available = kb << 10
		}
	}
	if total == 0 {
		return 0, 0, fmt.Errorf("no MemTotal in %s/meminfo", procRoot)
	}
	return total, available, nil
}

// CheckHealth asks the app's live container for its health check path on
// the project's network, on port; checked is false when there was nothing
// to ask (the container isn't running: WatchDeaths reports that).
func (Local) CheckHealth(ctx context.Context, app AppSpec, port int64) (ok bool, why string, checked bool) {
	container := app.Live()
	if deploy.ContainerState(ctx, container).Status != "running" {
		return false, "", false
	}
	ip, err := deploy.ContainerIP(ctx, container, ProjectNetwork(app.Project))
	if err != nil {
		return false, "", false
	}
	path := "/" + strings.TrimPrefix(app.HealthPath, "/")
	requireOK := path != "/"
	if deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, port, path), requireOK) {
		return true, "", true
	}
	if requireOK {
		return false, fmt.Sprintf("didn't return 2xx on port %d%s", port, path), true
	}
	return false, fmt.Sprintf("didn't answer on port %d", port), true
}
