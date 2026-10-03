package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/build"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/proxy"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func ctx() context.Context { return context.Background() }

// An app's images: :latest is live, :previous is the rollback target and
// :next is a build that hasn't passed its health check yet.
func ImageTag(app store.App) string         { return "hakobu/" + app.Name + ":latest" }
func PreviousImageTag(app store.App) string { return "hakobu/" + app.Name + ":previous" }
func nextImageTag(app store.App) string     { return "hakobu/" + app.Name + ":next" }

var (
	jobsMu  sync.Mutex
	running = map[string]bool{} // apps with a deploy, rollback or deletion in progress
	queued  = map[string]bool{} // a push arrived during a deploy; deploy again after it
	closed  string              // why no job may start now, "" when they may
)

// CloseJobs stops new deploys, rollbacks, deletions and database jobs from
// starting, saying why: while the agent starts up or as it stops.
func CloseJobs(why string) {
	jobsMu.Lock()
	closed = why
	jobsMu.Unlock()
}

func OpenJobs() { CloseJobs("") }

// JobsOpen reports whether jobs may start.
func JobsOpen() bool { return jobsClosed() == nil }

func jobsClosed() error {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if closed != "" {
		return errors.New(closed + ", try again in a minute")
	}
	return nil
}

func IsDeploying(name string) bool {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return running[name]
}

// reserve marks the app busy; with queuePush a busy app gets a follow-up
// push deploy instead of an error.
func reserve(name string, queuePush bool) (ok bool, err error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if closed != "" {
		return false, errors.New(closed + ", try again in a minute")
	}
	if running[name] {
		if queuePush {
			queued[name] = true
			return false, nil
		}
		return false, fmt.Errorf("a deploy of %s is in progress, try again when it finishes", name)
	}
	running[name] = true
	return true, nil
}

// WaitForJobs waits up to timeout for running deploys, rollbacks,
// deletions and database jobs to end, as the agent stops; false if some
// still run.
func WaitForJobs(timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); ; time.Sleep(500 * time.Millisecond) {
		jobsMu.Lock()
		n := len(running)
		jobsMu.Unlock()
		if n == 0 && !dbJobsRunning() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// release frees the app and reports whether a queued push should run now.
func release(name string) (runQueued bool) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	delete(running, name)
	runQueued = queued[name]
	delete(queued, name)
	return runQueued
}

// deployLog streams job output into its deploy_logs row, at most once a second.
// A long build's output is cut in the middle: the start says what ran, the
// end why it failed, and the row is rewritten whole on every flush.
type deployLog struct {
	s       *store.Store
	id      int64
	mu      sync.Mutex
	head    []byte // the first deployLogHead bytes
	tail    []byte // what came after, the last deployLogTail of it kept
	cut     int    // bytes dropped between the two
	flushed time.Time
}

const (
	deployLogHead = 64 << 10
	deployLogTail = 1 << 20
)

func (l *deployLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.add(p)
	if time.Since(l.flushed) > time.Second {
		_ = l.s.UpdateDeployLog(ctx(), store.UpdateDeployLogParams{ID: l.id, Status: "running", Output: secret.String(l.text())})
		l.flushed = time.Now()
	}
	return len(p), nil
}

func (l *deployLog) add(p []byte) {
	n := min(len(p), deployLogHead-len(l.head))
	l.head = append(l.head, p[:n]...)
	l.tail = append(l.tail, p[n:]...)
	if over := len(l.tail) - deployLogTail; over > deployLogTail { // trimmed in batches
		l.cut += over
		l.tail = append([]byte(nil), l.tail[over:]...)
	}
}

// text is the output so far; the caller holds mu.
func (l *deployLog) text() string {
	tail, cut := l.tail, l.cut
	if over := len(tail) - deployLogTail; over > 0 {
		tail, cut = tail[over:], cut+over
	}
	if cut == 0 {
		return string(l.head) + string(tail)
	}
	return fmt.Sprintf("%s\n[... %d bytes of output cut ...]\n%s", l.head, cut, tail)
}

func (l *deployLog) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := "success"
	if err != nil {
		status = "failed"
		l.add([]byte(fmt.Sprintln("error:", err)))
	}
	_ = l.s.UpdateDeployLog(ctx(), store.UpdateDeployLogParams{ID: l.id, Status: status, Output: secret.String(l.text())})
}

// startJob runs fn in the background with a deploy log; only one job per
// app runs at a time, and a push arriving meanwhile is deployed after it.
func startJob(s *store.Store, appName, trigger string, fn func(app store.App, out io.Writer) error) error {
	ok, err := reserve(appName, trigger == "push")
	if !ok {
		return err
	}
	app, err := s.GetApp(ctx(), appName)
	if err == nil {
		var id int64
		id, err = s.CreateDeployLog(ctx(), store.CreateDeployLogParams{AppName: appName, Trigger: trigger, Status: "running"})
		if err == nil {
			go func() {
				log := &deployLog{s: s, id: id}
				err := fn(app, log)
				log.finish(err)
				noteDeploy(s, appName, trigger, err)
				if release(appName) {
					StartDeploy(s, appName, "push")
				}
			}()
			return nil
		}
	}
	release(appName)
	return err
}

// StartDeploy clones the app's repo, builds a new image and rolls it out.
func StartDeploy(s *store.Store, appName, trigger string) error {
	return startJob(s, appName, trigger, func(app store.App, out io.Writer) error {
		token, err := repoToken(s, app.Repo)
		if err != nil {
			return err
		}
		workDir := workDir(app.Name)
		defer os.RemoveAll(workDir) // the next deploy clones again anyway
		fmt.Fprintln(out, "cloning", app.Repo)
		if err := build.CloneRepo(github.CloneURL(app.Repo), token, workDir, out); err != nil {
			return fmt.Errorf("clone failed: %w", err)
		}
		dir, err := buildDir(workDir, app.BuildPath)
		if err != nil {
			return err
		}
		stack := detect.StackOf(dir)

		next := nextImageTag(app)
		// Drops the :next tag in every case: a failed build or health check
		// leaves nothing behind, a successful one is :latest by then.
		defer deploy.RemoveImage(ctx(), next)
		fmt.Fprintf(out, "building %s from %s (%s)\n", next, dir, app.BuildStrategy)
		if err := build.BuildWithStrategy(dir, next, app.BuildStrategy, out); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
		snapshot := takeSnapshot(s, app, out)
		defer os.Remove(snapshot) // kept by keepSnapshot, which moves it
		if err := rollOut(s, app, next, out); err != nil {
			return err
		}
		if err := withSnapshot(s, app, snapshot, out, func() error { return promote(app, out) }); err != nil {
			return err
		}
		// For the panel's logo of the app, once this build is the live one;
		// a stale one is no reason to fail the deploy.
		if stack != app.Stack {
			if err := s.SetAppStack(ctx(), store.SetAppStackParams{Name: app.Name, Stack: stack}); err != nil {
				fmt.Fprintln(out, "warning: failed to note the app's stack:", err)
			}
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			if err := runWorker(s, app, w, out); err != nil {
				return fmt.Errorf("app deployed, but worker failed: %w", err)
			}
		}
		return nil
	})
}

// withSnapshot runs retag, which gives :previous a new image, and makes
// dump ("" for none) the snapshot that goes with it. The old snapshot is
// unpaired first: a failure or crash in between must leave no snapshot
// rather than pair the new :previous with data it never ran with.
func withSnapshot(s *store.Store, app store.App, dump string, out io.Writer, retag func() error) error {
	if err := s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{Name: app.Name}); err != nil {
		return err
	}
	if err := retag(); errors.Is(err, errPreviousNotKept) {
		fmt.Fprintln(out, "warning:", err)
		return nil // :previous didn't change, so no snapshot goes with it
	} else if err != nil {
		return err
	}
	if err := keepSnapshot(s, app.Name, app.LinkedDB, dump); err != nil {
		fmt.Fprintln(out, "warning: failed to keep the database snapshot, Rollback will only restore the code:", err)
		os.Remove(snapshotPath(app.Name))
	}
	return nil
}

// buildDir resolves the app's build path inside the clone. Symlinks are
// resolved first: a repo could otherwise make its build path point at a
// host directory and have it copied into the image.
func buildDir(clone, buildPath string) (string, error) {
	root, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, strings.Trim(buildPath, "/")))
	if err != nil {
		return "", fmt.Errorf("build path %q not found in the repo", buildPath)
	}
	if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid build path %q: it leads outside the repo", buildPath)
	}
	return dir, nil
}

// promote makes the :next build that just went live :latest; the image it
// replaced becomes the rollback target and the old rollback target is deleted.
// It returns errPreviousNotKept, once :latest is the new build, if the
// replaced image couldn't become :previous.
func promote(app store.App, out io.Writer) error {
	latest, prev := ImageTag(app), PreviousImageTag(app)
	dropped := deploy.ImageID(ctx(), prev)
	var kept error
	if ok, err := deploy.ImageExists(ctx(), latest); err != nil {
		kept = fmt.Errorf("%w: %v", errPreviousNotKept, err)
	} else if ok {
		if err := deploy.TagImage(ctx(), latest, prev); err != nil {
			kept = fmt.Errorf("%w: %v", errPreviousNotKept, err)
		}
	}
	if err := deploy.TagImage(ctx(), nextImageTag(app), latest); err != nil {
		return err
	}
	if dropped != "" && dropped != deploy.ImageID(ctx(), prev) && dropped != deploy.ImageID(ctx(), latest) {
		deploy.RemoveImage(ctx(), dropped)
	}
	return kept
}

var errPreviousNotKept = errors.New("failed to keep the previous image for rollback")

// StartRollback redeploys the image that was live before the current one;
// the two swap places, so rolling back again returns to where it started.
// withData also returns the database to its snapshot from before the last
// deploy, losing what was written since; the current data becomes the
// snapshot, so rolling back again returns it too.
func StartRollback(s *store.Store, appName string, withData bool) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if ok, err := deploy.ImageExists(ctx(), PreviousImageTag(app)); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no previous build to roll back to")
	}
	if reason := DataRollbackBlocker(s, app); withData && reason != "" {
		return fmt.Errorf("can't roll back the database: %s", reason)
	}
	trigger := "rollback"
	if withData {
		trigger = "rollback with data"
	}
	return startJob(s, appName, trigger, func(app store.App, out io.Writer) error {
		prev, latest, swap := PreviousImageTag(app), ImageTag(app), nextImageTag(app)
		defer deploy.RemoveImage(ctx(), swap)
		current := ""
		if withData {
			var err error
			if current, err = rollBackData(s, app, out); err != nil {
				return err
			}
			// Moved by keepSnapshot on success; kept if undoing fails.
			defer func() {
				if current != "" {
					os.Remove(current)
				}
			}()
		}
		if err := rollOut(s, app, prev, out); err != nil {
			if withData && !undoDataRollback(s, app, current, out) {
				current = ""
			}
			return err
		}
		// The snapshot must match the new :previous: the data it ran with,
		// or nothing after a code-only rollback.
		if err := withSnapshot(s, app, current, out, func() error {
			for _, t := range [][2]string{{latest, swap}, {prev, latest}, {swap, prev}} {
				if err := deploy.TagImage(ctx(), t[0], t[1]); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			return runWorker(s, app, w, out)
		}
		return nil
	})
}

// rollBackData saves the current data, stops the app and its worker so
// nothing writes during the swap, and puts the snapshot in place. It
// returns the dump of the current data.
func rollBackData(s *store.Store, app store.App, out io.Writer) (current string, err error) {
	d, err := s.GetDatabase(ctx(), app.LinkedDB)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(out, "saving the current data of", d.Name)
	if current, err = dumpTo(d); err != nil {
		return "", fmt.Errorf("couldn't save the current data, nothing changed: %w", err)
	}
	fmt.Fprintln(out, "stopping", app.Name, "and restoring", d.Name, "from", app.SnapshotAt)
	err = stopApp(app)
	if err == nil {
		err = replaceDatabase(d, snapshotPath(app.Name))
	}
	if err != nil {
		os.Remove(current)
		restartContainer(app, app.ContainerName(), out)
		restartWorker(s, app, out)
		return "", fmt.Errorf("the data is unchanged: %w", err)
	}
	return current, nil
}

// undoDataRollback puts the saved current data back when the previous
// version didn't start, then starts the current version again. If it
// can't, the dump is kept on disk and it returns false.
func undoDataRollback(s *store.Store, app store.App, current string, out io.Writer) bool {
	err := stopApp(app) // a recreate deploy may have started it again already
	if err == nil {
		var d store.Database
		if d, err = s.GetDatabase(ctx(), app.LinkedDB); err == nil {
			err = replaceDatabase(d, current)
		}
	}
	if err != nil {
		keep := filepath.Join(snapshotDir, app.Name+"-before-rollback"+snapshotExt)
		if rerr := os.Rename(current, keep); rerr != nil {
			keep = current
		}
		fmt.Fprintf(out, "ERROR: couldn't put the current data back (%v); it's saved in %s\n", err, keep)
		return false
	}
	fmt.Fprintln(out, "put the current data back")
	restartContainer(app, app.ContainerName(), out)
	restartWorker(s, app, out)
	return true
}

// stopApp stops the app and its worker so nothing writes to the database.
func stopApp(app store.App) error {
	if err := deploy.StopContainer(ctx(), app.ContainerName()); err != nil {
		return fmt.Errorf("couldn't stop %s: %w", app.Name, err)
	}
	if err := deploy.StopContainer(ctx(), app.Name+"-worker"); err != nil {
		return fmt.Errorf("couldn't stop the worker of %s: %w", app.Name, err)
	}
	return nil
}

func restartWorker(s *store.Store, app store.App, out io.Writer) {
	if w, err := s.GetWorker(ctx(), app.Name); err == nil {
		if err := runWorker(s, app, w, out); err != nil {
			fmt.Fprintln(out, "failed to start the worker again:", err)
		}
	}
}

// rollOut is the blue/green swap: start the inactive slot, health-check it
// on the docker network, point the proxy at it, then remove the old slot.
// If the candidate never gets healthy the old slot keeps serving.
//
// An app with volumes is recreated instead, unless it shares them: the old
// version is stopped first so two versions never write the same data, and
// started again if the new one fails.
func rollOut(s *store.Store, app store.App, imageTag string, out io.Writer) error {
	hint := portHint(app, int64(deploy.ExposedPort(ctx(), imageTag)))
	env, err := appEnv(s, app, hint)
	if err != nil {
		return err
	}
	oldSlot, newSlot := "blue", "green"
	if app.ActiveSlot == "green" {
		oldSlot, newSlot = "green", "blue"
	}
	candidate, old := app.Name+"-"+newSlot, app.Name+"-"+oldSlot

	binds, err := appBinds(s, app.Name)
	if err != nil {
		return err
	}
	recreate := len(binds) > 0 && app.ShareVolumes == 0
	if recreate {
		fmt.Fprintln(out, "the app has volumes: stopping", old, "before starting the new version")
		if err := deploy.StopContainer(ctx(), old); err != nil {
			return err
		}
	}
	restoreOld := func() {
		if recreate {
			restartContainer(app, old, out)
		}
	}

	if err := ensureProjectNetworks(s, app.ProjectName); err != nil {
		restoreOld()
		return err
	}
	fmt.Fprintln(out, "starting", candidate)
	if _, err := deploy.RunAppContainer(ctx(), imageTag, candidate, appOptions(app, env, binds)); err != nil {
		restoreOld()
		return err
	}
	ip, err := deploy.ContainerIP(ctx(), candidate, ProjectNetwork(app.ProjectName))
	if err != nil {
		deploy.RemoveContainer(ctx(), candidate)
		restoreOld()
		return err
	}
	path := "/" + strings.TrimPrefix(app.HealthCheckPath, "/")
	requireOK := path != "/"
	if app.ContainerPort > 0 {
		fmt.Fprintf(out, "waiting for port %d to answer %s...\n", app.ContainerPort, path)
	} else {
		fmt.Fprintf(out, "waiting for the app to listen on a port (PORT=%d) and answer %s...\n", hint, path)
	}

	var port int64
	var loopbackOnly []int
	var crashed, oom, killed bool
	healthy := deploy.WaitHealthy(60, time.Second, func() bool {
		// A container that exits or restarts will never answer; stop waiting.
		if st := deploy.ContainerState(ctx(), candidate); st.Status == "exited" || st.Status == "dead" || st.Restarts > 0 || st.OOMKilled {
			st = awaitOOMFlag(app, candidate, st)
			crashed, oom, killed = true, st.OOMKilled, st.ExitCode == 137
			return true
		}
		port = app.ContainerPort
		if port == 0 {
			var reachable []int
			reachable, loopbackOnly, _ = deploy.ListeningPorts(ctx(), candidate)
			port = pickPort(reachable, hint)
		}
		return port > 0 && deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, port, path), requireOK)
	})
	if !healthy || crashed {
		logs, _ := deploy.ContainerLogs(ctx(), candidate, 50)
		deploy.RemoveContainer(ctx(), candidate)
		restoreOld()
		reason := fmt.Sprintf("didn't answer on port %d within 60s", port)
		switch {
		case oom:
			reason = "was killed: " + oomText(app)
		case killed && app.MemoryMB > 0:
			reason = "was killed (exit 137), most likely because " + oomText(app)
		case crashed:
			reason = "exited while starting (see its output below; a missing variable or an unreachable database are the usual causes)"
		case port == 0 && len(loopbackOnly) > 0:
			reason = fmt.Sprintf("listens only on 127.0.0.1 (port %v); bind it to 0.0.0.0", loopbackOnly)
		case port == 0:
			reason = "didn't listen on any port within 60s"
		case requireOK:
			reason = fmt.Sprintf("didn't return 2xx on port %d%s within 60s", port, path)
		}
		kept := "previous version keeps running"
		if recreate {
			kept = "previous version was started again"
		}
		return fmt.Errorf("new version %s; %s\n--- container output ---\n%s", reason, kept, logs)
	}

	// Healthy: keep it running, and let the tunnel find it under the app's
	// alias next to the old version. Until the proxy points at it, any
	// failure removes it and leaves the old version serving: two versions
	// left answering under the alias would split the traffic.
	abandon := func(err error) error {
		if rerr := deploy.RemoveContainer(ctx(), candidate); rerr != nil {
			fmt.Fprintln(out, "warning:", rerr)
		}
		restoreOld()
		return err
	}
	if err := deploy.KeepRestarting(ctx(), candidate); err != nil {
		return abandon(err)
	}
	if err := deploy.ConnectNetwork(ctx(), candidate, projectEdge(app.ProjectName), EdgeAlias(app.Name)); err != nil {
		return abandon(err)
	}
	if _, err := proxy.Ensure(app.Name, app.Port); err != nil {
		return abandon(err)
	}
	// The database first: after a crash between the two, the agent points
	// the proxy at the slot the database names (EnsureProxy), and
	// ReconcileSlots removes the other one.
	if err := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: app.Name, ActiveSlot: newSlot, LivePort: port}); err != nil {
		return abandon(err)
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, port))
	if err := proxy.SetTarget(app.Name, u); err != nil {
		if rerr := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: app.Name, ActiveSlot: app.ActiveSlot, LivePort: app.LivePort}); rerr != nil {
			fmt.Fprintln(out, "warning:", rerr)
		}
		return abandon(err)
	}
	if err := SyncTunnel(s); err != nil {
		fmt.Fprintln(out, "warning: failed to update the tunnel's routes, retrying in the background:", err)
	}
	// The old version gets no new requests, then up to 10 seconds to
	// finish the ones in flight.
	if err := deploy.DisconnectNetwork(ctx(), old, projectEdge(app.ProjectName)); err != nil {
		fmt.Fprintln(out, "warning:", err)
	}
	if err := deploy.StopContainer(ctx(), old); err != nil {
		fmt.Fprintln(out, "warning:", err) // removing it below kills it anyway
	}
	if err := deploy.RemoveContainer(ctx(), old); err != nil {
		fmt.Fprintln(out, "warning: failed to remove previous container:", err)
	}
	fmt.Fprintf(out, "live: container port %d, 127.0.0.1:%d (%s)\n", port, app.Port, imageTag)
	return nil
}

// awaitOOMFlag rereads a stopped container's state for up to a second when
// it may have run out of memory but isn't flagged yet: Docker can report the
// exit before the OOM kill (seen under rootless Docker). Only a container
// with a memory limit gets the flag, so others aren't kept waiting.
func awaitOOMFlag(app store.App, container string, st deploy.State) deploy.State {
	for i := 0; i < 5 && !st.OOMKilled && app.MemoryMB > 0; i++ {
		time.Sleep(200 * time.Millisecond)
		st = deploy.ContainerState(ctx(), container)
	}
	return st
}

// restartContainer brings a stopped previous version back after a failed
// recreate deploy and points the proxy at it again.
func restartContainer(app store.App, name string, out io.Writer) {
	if status, _ := deploy.ContainerStatus(ctx(), name); status == "not found" {
		return
	}
	if err := deploy.StartContainer(ctx(), name); err != nil {
		fmt.Fprintln(out, "failed to start the previous version again:", err)
		return
	}
	if ip, err := deploy.ContainerIP(ctx(), name, ProjectNetwork(app.ProjectName)); err == nil {
		u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, portHint(app, 0)))
		proxy.SetTarget(app.Name, u)
	}
	fmt.Fprintln(out, "started the previous version again:", name)
}

// portHint is the PORT the app is told to listen on: the configured port,
// else the one it used last time, else the image's EXPOSE, else 8080.
func portHint(app store.App, exposed int64) int64 {
	for _, p := range []int64{app.ContainerPort, app.LivePort, exposed} {
		if p > 0 {
			return p
		}
	}
	return 8080
}

// pickPort prefers the hinted port among the listening ones.
func pickPort(listening []int, hint int64) int64 {
	for _, p := range listening {
		if int64(p) == hint {
			return hint
		}
	}
	if len(listening) > 0 {
		return int64(listening[0])
	}
	return 0
}

// EnsureProxy restarts an app's proxy after an agent restart and points it
// at the active container.
func EnsureProxy(app store.App) {
	isNew, err := proxy.Ensure(app.Name, app.Port)
	if err != nil || !isNew {
		return
	}
	ip, err := deploy.ContainerIP(ctx(), app.ContainerName(), ProjectNetwork(app.ProjectName))
	if err != nil {
		return
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, portHint(app, 0)))
	proxy.SetTarget(app.Name, u)
}

func RemoveProxy(name string) { proxy.Remove(name) }

// ReconcileSlots tidies up after a job the agent didn't live to finish, run
// at startup before any job: the slot the database doesn't name is a
// leftover candidate or old version (still answering under the app's edge
// alias), and a stopped live container was stopped by a recreate deploy or
// data rollback that never started it again.
func ReconcileSlots(s *store.Store) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		fmt.Println("failed to check the apps' containers:", err)
		return
	}
	for _, app := range apps {
		if IsDeploying(app.Name) {
			continue
		}
		live := app.ContainerName()
		status, _ := deploy.ContainerStatus(ctx(), live)
		if status == "not found" || status == "unknown" {
			continue // never deployed, or Docker isn't answering: leave both alone
		}
		other := app.Name + "-green"
		if live == other {
			other = app.Name + "-blue"
		}
		if st, _ := deploy.ContainerStatus(ctx(), other); st != "not found" && st != "unknown" {
			fmt.Println("removing", other+", left by an unfinished deploy of", app.Name)
			if err := deploy.RemoveContainer(ctx(), other); err != nil {
				fmt.Println("failed to remove", other+":", err)
			}
		}
		if status == "exited" || status == "created" {
			fmt.Println("starting", live+", stopped by an unfinished deploy")
			if err := deploy.StartContainer(ctx(), live); err != nil {
				fmt.Println("failed to start", live+":", err)
			}
		}
	}
}

// Workers.

func runWorker(s *store.Store, app store.App, w store.Worker, out io.Writer) error {
	if err := ensureProjectNetworks(s, app.ProjectName); err != nil {
		return err
	}
	env, err := WorkerEnv(s, app, w)
	if err != nil {
		return err
	}
	binds, err := appBinds(s, app.Name)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "starting worker", w.ContainerName())
	_, err = deploy.RunWorkerContainer(ctx(), ImageTag(app), w.ContainerName(), string(w.Command), appOptions(app, env, binds))
	return err
}

func appOptions(app store.App, env, binds []string) deploy.AppOptions {
	return deploy.AppOptions{App: app.Name, Network: ProjectNetwork(app.ProjectName), Env: env, Binds: binds, MemoryMB: app.MemoryMB, CPUs: app.Cpus}
}

// SaveWorker creates or updates the app's worker and restarts it if the
// app has been built.
func SaveWorker(s *store.Store, w store.Worker) error {
	app, err := s.GetApp(ctx(), w.AppName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(w.Command)) == "" {
		return fmt.Errorf("worker command is required")
	}
	if err := checkNotSealed(s, "worker", w.AppName, string(w.Env)); err != nil {
		return err
	}
	if err := s.SaveWorker(ctx(), store.SaveWorkerParams(w)); err != nil {
		return err
	}
	return RestartWorker(s, app)
}

func RestartWorker(s *store.Store, app store.App) error {
	w, err := s.GetWorker(ctx(), app.Name)
	if err != nil {
		return fmt.Errorf("%s has no worker", app.Name)
	}
	if ok, _ := deploy.ImageExists(ctx(), ImageTag(app)); !ok {
		return nil // started by the first successful deploy
	}
	return runWorker(s, app, w, io.Discard)
}

func DeleteWorker(s *store.Store, appName string) error {
	if err := deploy.RemoveContainer(ctx(), appName+"-worker"); err != nil {
		return err
	}
	if err := s.DeleteSealedVarsOf(ctx(), store.DeleteSealedVarsOfParams{Scope: "worker", Owner: appName}); err != nil {
		return err
	}
	return s.DeleteWorker(ctx(), appName)
}
