package ops

import (
	"fmt"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
)

// buildCacheKeep is how long unused build cache is kept: long enough that
// redeploys stay fast, short enough that the disk doesn't fill up.
const buildCacheKeep = 7 * 24 * time.Hour

var (
	lastCleanupMu sync.Mutex
	lastCleanup   string
)

// LastCleanup describes the most recent Cleanup run, "" if none yet.
func LastCleanup() string {
	lastCleanupMu.Lock()
	defer lastCleanupMu.Unlock()
	return lastCleanup
}

// Cleanup frees disk: build cache unused for a week, and images, clones and
// volumes left over from apps or volumes that no longer exist.
func Cleanup(s *store.Store) error {
	result, err := cleanup(s)
	if err != nil {
		result = "failed: " + err.Error()
	}
	lastCleanupMu.Lock()
	lastCleanup = time.Now().Format("2006-01-02 15:04") + ": " + result
	lastCleanupMu.Unlock()
	return err
}

func cleanup(s *store.Store) (string, error) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		return "", err
	}
	vols, err := s.ListAllVolumes(ctx())
	if err != nil {
		return "", err
	}
	places, err := projectPlaces(s)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		if _, ok := places[a.ProjectID]; !ok {
			// Its server is unknown: every server would take its volumes
			// for leftovers.
			return "", fmt.Errorf("the project of app %s not found", a.Name)
		}
	}
	var freed int64
	var images int
	for _, n := range allNodes(s) {
		// A server hears only of its own apps: it could belong to someone
		// else.
		spec := node.CleanupSpec{Apps: map[string]bool{}, Busy: map[string]bool{}, Volumes: map[string]bool{}, CacheKeep: buildCacheKeep}
		mine := map[string]bool{}
		for _, a := range apps {
			if places[a.ProjectID].server == n.ID {
				mine[a.Name] = true
				spec.Apps[a.Name] = true
				spec.Busy[a.Name] = IsDeploying(a.Name)
			}
		}
		for _, v := range vols {
			if mine[v.AppName] {
				spec.Volumes[node.Volume(v.AppName, v.Name)] = true
			}
		}
		f, i, err := n.n.Cleanup(ctx(), spec)
		freed, images = freed+f, images+i
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("freed %s of build cache, removed %d image(s) of deleted apps", humanBytes(uint64(freed)), images), nil
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// DiskUsage describes the disk Docker uses, e.g. "41.2 GB of 98.3 GB (42%)";
// low is true below 10% free.
func DiskUsage() (text string, low bool) {
	used, total, err := local.Disk(ctx())
	if err != nil || total == 0 {
		return "unknown", false
	}
	pct := used * 100 / total
	return fmt.Sprintf("%s of %s used (%d%%)", humanBytes(used), humanBytes(total), pct), pct >= 90
}
