package ops

import (
	"fmt"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// healthFailures is how many checks in a row an app fails before it counts
// as down: one slow answer during a garbage collection isn't an outage.
const healthFailures = 3

// failedChecks counts the failed checks in a row of each app; an app is
// down from healthFailures on.
var failedChecks struct {
	sync.Mutex
	n map[int64]int // by app ID
}

// WatchHealth checks every live app the way its deploy did, on the docker
// network, and records an app that stops answering in its Errors tab and
// emails the owner, and again when it answers again. A container that
// isn't running is left to WatchDeaths, which reports the crash.
func WatchHealth(s *store.Store) {
	for {
		time.Sleep(config.HealthCheckEvery)
		apps, err := s.ListApps(ctx())
		if err != nil {
			continue
		}
		var wg sync.WaitGroup
		for _, app := range apps {
			if app.LivePort == 0 || IsDeploying(app.ID) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, why, checked := checkHealth(s, app); checked {
					noteHealth(s, app, ok, why)
				}
			}()
		}
		wg.Wait()
		forgetHealth(apps)
	}
}

// checkHealth asks the live container of app for its health check path on
// its node; checked is false when there was nothing to ask.
func checkHealth(s *store.Store, app store.App) (ok bool, why string, checked bool) {
	return AppNode(s, app).CheckHealth(ctx(), liveSpec(app), app.LivePort)
}

// noteHealth counts a check of app and records the change when the app
// goes down or comes back.
func noteHealth(s *store.Store, app store.App, ok bool, why string) {
	failedChecks.Lock()
	if failedChecks.n == nil {
		failedChecks.n = map[int64]int{}
	}
	before := failedChecks.n[app.ID]
	if ok {
		delete(failedChecks.n, app.ID)
	} else {
		failedChecks.n[app.ID]++
	}
	wasDown, isDown := before >= healthFailures, failedChecks.n[app.ID] >= healthFailures
	failedChecks.Unlock()

	key := fmt.Sprintf("health:%d", app.ID)
	switch {
	case isDown && !wasDown:
		message := fmt.Sprintf("%s %s %d times in a row: it's running but not serving.", app.Name, why, healthFailures)
		recordHealth(s, app, "error", message)
		problem(s, key, notifyAgain, app.Name+": not responding",
			message+"\n\nIts output: "+panelURL(AppPath(app)+"/logs"))
	case wasDown && !isDown:
		recordHealth(s, app, "info", app.Name+" answers its health check again.")
		solved(s, key, app.Name+": responding again", app.Name+" answers its health check again.\n\n"+panelURL(AppPath(app)))
	}
}

func recordHealth(s *store.Store, app store.App, level, message string) {
	if err := addAppEvent(s, teldb.CreateTelemetryEventParams{
		AppID: app.ID, Kind: "health", Level: level, Message: secret.String(message),
	}); err != nil {
		panellog.Error("failed to record the health of", app.Name+":", err)
	}
}

// forgetHealth drops the counts of deleted apps.
func forgetHealth(apps []store.App) {
	live := map[int64]bool{}
	for _, a := range apps {
		live[a.ID] = true
	}
	failedChecks.Lock()
	defer failedChecks.Unlock()
	for id := range failedChecks.n {
		if !live[id] {
			delete(failedChecks.n, id)
		}
	}
}
