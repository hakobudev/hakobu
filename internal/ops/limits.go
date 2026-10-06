package ops

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// SetLimits caps the memory and CPUs of the app and its worker from the
// next deploy on; 0 removes a limit.
func SetLimits(s *store.Store, appID int64, memoryMB int64, cpus float64) error {
	if memoryMB != 0 && memoryMB < 16 {
		return fmt.Errorf("memory limit must be at least 16 MB, or empty for no limit")
	}
	if n := runtime.NumCPU(); cpus < 0 || cpus > float64(n) {
		return fmt.Errorf("CPU limit must be between 0.01 and %d (the server's CPUs), or empty for no limit", n)
	}
	if cpus != 0 && cpus < 0.01 {
		return fmt.Errorf("CPU limit must be at least 0.01")
	}
	return s.SetAppLimits(ctx(), store.SetAppLimitsParams{ID: appID, MemoryMB: memoryMB, Cpus: cpus})
}

func oomText(app store.App) string { return node.OOMText(app.MemoryMB) }

// WatchDeaths records every out-of-memory kill of an app or worker
// container, and every crash of a live one, in the app's Errors tab and
// emails the owner, reconnecting to Docker whenever the stream breaks.
//
// This watches the panel's server; a server that joined is watched while
// it's connected (watchServer).
func WatchDeaths(s *store.Store) {
	for {
		err := local.WatchDeaths(context.Background(), func(container, app string, d node.Death) { recordDeath(s, server{}, container, app, d) })
		panellog.Warn("docker events:", err)
		time.Sleep(5 * time.Second)
	}
}

// recordDeath puts an out-of-memory kill of an app or worker container, or
// a crash of a live one, in the app's Errors tab and emails the owner.
// appKey is the app's on its server (store.AppKey).
func recordDeath(s *store.Store, sv server, container, appKey string, d node.Death) {
	id, known := store.ParseAppKey(appKey)
	app, err := s.GetAppByID(ctx(), id)
	if on, ok := appServer(s, app); known && err == nil && (!ok || on.ID != sv.ID) {
		// A server tells only of its own apps: it could belong to
		// someone else.
		return
	}
	if !known || err != nil {
		if d.OOM {
			panellog.Warn(container, "was killed, most likely out of memory")
		}
		return
	}
	var kind, message string
	switch {
	case d.OOM && d.Sure:
		kind, message = "oom", fmt.Sprintf("%s was killed: %s. Docker restarts it.", app.Name, oomText(app))
	case d.OOM:
		kind, message = "oom", fmt.Sprintf("%s was killed (exit 137), most likely because %s. Docker restarts it.", app.Name, oomText(app))
	case container == app.ContainerName() || container == node.WorkerContainer(app.Key()):
		// A candidate that crashes while starting fails its deploy,
		// which says so itself.
		kind, message = "crash", fmt.Sprintf("%s exited with code %s. Docker restarts it; its output says why.", app.Name, d.ExitCode)
	default:
		return
	}
	if strings.HasSuffix(container, "-worker") {
		message = "The worker of " + message
	}
	if err := addAppEvent(s, teldb.CreateTelemetryEventParams{
		AppID: app.ID, Kind: kind, Level: "fatal", Message: secret.String(message),
	}); err != nil {
		panellog.Error("failed to record that", app.Name, "stopped:", err)
	}
	if kind == "oom" {
		noteOOM(s, app, message)
	} else {
		noteCrash(s, app, message)
	}
}

// LastOOM is when the app or its worker last ran out of memory, "" if not
// within the retention period.
func LastOOM(s *store.Store, app store.App) string {
	t, err := appTelemetry(s, app)
	if err != nil {
		return ""
	}
	at, _ := t.LastAppEvent(ctx(), app.ID, "oom")
	return at
}
