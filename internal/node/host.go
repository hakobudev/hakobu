package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/deploy"
)

// The node's machine: what it uses, cleaning up after deleted apps, and
// what the panel watches it for (containers dying, apps not answering).
// The node only reads and reports; the panel keeps the history, works out
// rates and decides what to email.

// Counters are a container's totals since it started.
type Counters = deploy.Counters

// Death is why a container stopped by itself.
type Death = deploy.Death

// HostCounters are the machine's totals and levels when read.
type HostCounters struct {
	CPUBusy, CPUTotal uint64 // clock ticks since boot; idle and iowait aren't busy
	CPUs              int
	MemTotal, MemFree int64 // MemFree is what's available, in bytes
	Load              float64
	DiskUsed          uint64 // of the disk Docker uses
	DiskTotal         uint64
}

// ContainerReading is a running container's counters.
type ContainerReading struct {
	ID, Name string
	Counters
}

// Readings is the machine's counters and those of the running containers
// asked for. HostErr says why there are none of the machine's.
type Readings struct {
	Host       HostCounters
	HostErr    string
	Containers []ContainerReading
}

// Readings reads the machine's counters and those of the running
// containers among names; it fails only when Docker doesn't answer.
func (Local) Readings(ctx context.Context, names map[string]bool) (Readings, error) {
	var r Readings
	var err error
	if r.Host, err = readHost(ctx); err != nil {
		r.HostErr = err.Error()
	}
	running, err := deploy.RunningContainers(ctx)
	if err != nil {
		return r, err
	}
	for _, c := range running {
		if !names[c.Name] {
			continue
		}
		counters, err := deploy.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		r.Containers = append(r.Containers, ContainerReading{ID: c.ID, Name: c.Name, Counters: counters})
	}
	return r, nil
}

// Disk is the use of the disk Docker keeps its data on.
func (Local) Disk(ctx context.Context) (used, total uint64, err error) {
	return deploy.Disk(ctx)
}

// procRoot is where the kernel's /proc is; tests point it at fixtures.
var procRoot = "/proc"

func readHost(ctx context.Context) (HostCounters, error) {
	var h HostCounters
	var err error
	if h.CPUBusy, h.CPUTotal, h.CPUs, err = readCPUTimes(); err != nil {
		return h, err
	}
	h.MemTotal, h.MemFree, _ = readMemInfo()
	h.Load, _ = readLoad()
	h.DiskUsed, h.DiskTotal, _ = deploy.Disk(ctx)
	return h, nil
}

// readCPUTimes reads the first line of /proc/stat; idle and iowait are
// the time not busy.
func readCPUTimes() (busy, total uint64, cpus int, err error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			cpus++
			continue
		}
		for i, v := range fields[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, 0, 0, err
			}
			if i >= 8 { // guest time is already in user and nice
				break
			}
			total += n
			if i != 3 && i != 4 { // idle, iowait
				busy += n
			}
		}
	}
	if total == 0 || cpus == 0 {
		return 0, 0, 0, fmt.Errorf("no CPU times in %s/stat", procRoot)
	}
	return busy, total, cpus, sc.Err()
}

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

// readLoad reads the one-minute load average.
func readLoad() (float64, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "loadavg"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty %s/loadavg", procRoot)
	}
	return strconv.ParseFloat(fields[0], 64)
}

// CleanupSpec says what on the node is still in use.
type CleanupSpec struct {
	Apps      map[string]bool // that exist
	Busy      map[string]bool // with a job running, whose clone stays
	Volumes   map[string]bool // by Volume name, that apps still have
	CacheKeep time.Duration   // build cache unused for longer goes
}

// Cleanup frees disk: build cache unused for a while, and images, clones,
// snapshots and volumes left over from apps or volumes that no longer
// exist. It returns the build cache freed and the images removed.
func (n Local) Cleanup(ctx context.Context, c CleanupSpec) (freed int64, images int, err error) {
	tags, err := deploy.ImageTags(ctx, "hakobu/*")
	if err != nil {
		return 0, 0, err
	}
	for _, tag := range tags {
		name := strings.TrimPrefix(tag[:strings.LastIndex(tag, ":")], "hakobu/")
		if !c.Apps[name] {
			_ = deploy.RemoveImage(ctx, tag)
			images++
		}
	}
	if clones, err := os.ReadDir("data/work"); err == nil {
		for _, clone := range clones {
			if !c.Busy[clone.Name()] {
				os.RemoveAll(WorkDir(clone.Name()))
			}
		}
	}
	n.PruneSnapshots(c.Apps)
	if err := n.RemoveVolumesExcept(ctx, c.Volumes); err != nil {
		return 0, images, err
	}
	freed, err = deploy.PruneBuildCache(ctx, c.CacheKeep)
	return max(freed, 0), images, err
}

// WatchDeaths calls fn for each container that stops by itself, with the
// app it belongs to ("" for none), until ctx ends or Docker's event
// stream breaks.
func (Local) WatchDeaths(ctx context.Context, fn func(container, app string, d Death)) error {
	return deploy.WatchDeaths(ctx, fn)
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
