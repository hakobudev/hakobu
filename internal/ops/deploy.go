package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/github"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func ctx() context.Context { return context.Background() }

// An app's images: :latest is live, :previous is the rollback target and
// :next is a build that hasn't passed its health check yet.
func ImageTag(app store.App) string         { return node.Tag(app.Name, node.Latest) }
func PreviousImageTag(app store.App) string { return node.Tag(app.Name, node.Previous) }
func nextImageTag(app store.App) string     { return node.Tag(app.Name, node.Next) }

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

// StartDeploy builds the app's repo on its node and rolls the build out.
func StartDeploy(s *store.Store, appName, trigger string) error {
	return startJob(s, appName, trigger, func(app store.App, out io.Writer) error {
		token, err := repoToken(s, app.Repo)
		if err != nil {
			return err
		}
		n := AppNode(s, app)
		// Drops the Next build in every case: a failed build or health
		// check leaves nothing behind, a successful one is Latest by then;
		// unless it's live and still to become Latest (pending below).
		pending := false
		defer func() {
			if !pending {
				n.DropImage(ctx(), app.Name, node.Next)
			}
		}()
		env, err := buildEnv(s, app)
		if err != nil {
			return err
		}
		stack, err := n.Build(ctx(), node.BuildSpec{
			App: app.Name, Repo: app.Repo, CloneURL: github.CloneURL(app.Repo), Token: token, Path: app.BuildPath, Strategy: app.BuildStrategy,
			Env: env,
		}, out)
		if err != nil {
			return err
		}
		snapshot := takeSnapshot(s, app, out)
		defer n.DropDump(snapshot) // kept by keepSnapshot, which moves it
		if err := rollOut(s, app, node.Next, out); err != nil {
			return err
		}
		if err := withSnapshot(s, app, snapshot, out, func() error { return promote(s, app) }); errors.Is(err, node.ErrUnreachable) {
			// The new version serves, but its server dropped off before
			// it became Latest there; that's finished as it connects
			// again (finishPromotions), so Latest is what serves.
			if perr := s.AddPendingPromotion(ctx(), app.Name); perr != nil {
				return errors.Join(err, perr)
			}
			pending = true
			fmt.Fprintln(out, "the new version is live; its server dropped off before keeping it as the latest build, which is done when it's back:", err)
		} else if err != nil {
			return err
		}
		// For the panel's logo of the app, once this build is the live one;
		// a stale one is no reason to fail the deploy.
		if stack != app.Stack {
			if err := s.SetAppStack(ctx(), store.SetAppStackParams{Name: app.Name, Stack: stack}); err != nil {
				fmt.Fprintln(out, "warning: failed to note the app's stack:", err)
			}
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil && !pending {
			if err := runWorker(s, app, w, out); err != nil {
				return fmt.Errorf("app deployed, but worker failed: %w", err)
			}
		}
		return nil
	})
}

// finishPromotions makes the live build Latest on a server that dropped
// off mid-deploy (see StartDeploy), then drops its Next tag and starts the
// app's worker on it.
func finishPromotions(s *store.Store, sv server, n node.Node) {
	apps, err := s.ListPendingPromotions(ctx())
	if err != nil {
		return
	}
	for _, name := range apps {
		app, err := s.GetApp(ctx(), name)
		if err != nil {
			continue
		}
		p, err := s.GetProject(ctx(), app.ProjectName)
		if err != nil {
			continue
		}
		if on, err := projectServer(s, p); err != nil || on.ID != sv.ID {
			continue
		}
		// No Next: the server promoted it before its answer was lost.
		if next, err := n.HasImage(ctx(), app.Name, node.Next); err != nil {
			continue
		} else if next {
			if err := n.Promote(ctx(), app.Name); err != nil && !errors.Is(err, node.ErrPreviousNotKept) {
				panellog.Error("keeping the live build of", app.Name, "as the latest:", err)
				continue
			}
			n.DropImage(ctx(), app.Name, node.Next)
		}
		if err := s.DeletePendingPromotion(ctx(), app.Name); err != nil {
			panellog.Error(app.Name+":", err)
		}
		if w, err := s.GetWorker(ctx(), app.Name); err == nil {
			if err := runWorker(s, app, w, io.Discard); err != nil {
				panellog.Error("starting the worker of", app.Name+":", err)
			}
		}
		panellog.Info("kept the live build of", app.Name, "as the latest on", sv.label())
	}
}

// withSnapshot runs retag, which gives the Previous build a new image, and
// makes dump ("" for none) the snapshot that goes with it. The old
// snapshot is unpaired first: a failure or crash in between must leave no
// snapshot rather than pair the new Previous with data it never ran with.
func withSnapshot(s *store.Store, app store.App, dump node.Dump, out io.Writer, retag func() error) error {
	if err := s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{Name: app.Name}); err != nil {
		return err
	}
	if err := retag(); errors.Is(err, node.ErrPreviousNotKept) {
		fmt.Fprintln(out, "warning:", err)
		return nil // Previous didn't change, so no snapshot goes with it
	} else if err != nil {
		return err
	}
	if err := keepSnapshot(s, app, app.LinkedDB, dump); err != nil {
		fmt.Fprintln(out, "warning: failed to keep the database snapshot, Rollback will only restore the code:", err)
		_ = AppNode(s, app).KeepSnapshot(app.Name, "") // drops it
	}
	return nil
}

// promote makes the Next build that just went live Latest on the app's
// node (see node.Local.Promote).
func promote(s *store.Store, app store.App) error {
	return AppNode(s, app).Promote(ctx(), app.Name)
}

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
	if ok, err := AppNode(s, app).HasImage(ctx(), app.Name, node.Previous); err != nil {
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
		n := AppNode(s, app)
		var current node.Dump
		if withData {
			var err error
			if current, err = rollBackData(s, app, out); err != nil {
				return err
			}
			// Moved by keepSnapshot on success; kept if undoing fails.
			defer func() { n.DropDump(current) }()
		}
		if err := rollOut(s, app, node.Previous, out); err != nil {
			if withData && !undoDataRollback(s, app, current, out) {
				current = ""
			}
			return err
		}
		// The snapshot must match the new Previous: the data it ran with,
		// or nothing after a code-only rollback.
		if err := withSnapshot(s, app, current, out, func() error { return n.SwapForRollback(ctx(), app.Name) }); err != nil {
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
func rollBackData(s *store.Store, app store.App, out io.Writer) (current node.Dump, err error) {
	d, err := s.GetDatabase(ctx(), app.LinkedDB)
	if err != nil {
		return "", err
	}
	n := AppNode(s, app)
	fmt.Fprintln(out, "saving the current data of", d.Name)
	if current, err = n.SaveDump(ctx(), dbSpec(d)); err != nil {
		return "", fmt.Errorf("couldn't save the current data, nothing changed: %w", err)
	}
	fmt.Fprintln(out, "stopping", app.Name, "and restoring", d.Name, "from", app.SnapshotAt)
	err = stopApp(s, app)
	if err == nil {
		err = n.ReplaceDatabase(ctx(), dbSpec(d), node.SnapshotOf(app.Name))
	}
	if err != nil {
		n.DropDump(current)
		startApp(s, app, out)
		restartWorker(s, app, out)
		return "", fmt.Errorf("the data is unchanged: %w", err)
	}
	return current, nil
}

// undoDataRollback puts the saved current data back when the previous
// version didn't start, then starts the current version again. If it
// can't, the dump is kept on disk and it returns false.
func undoDataRollback(s *store.Store, app store.App, current node.Dump, out io.Writer) bool {
	n := AppNode(s, app)
	err := stopApp(s, app) // a recreate deploy may have started it again already
	if err == nil {
		var d store.Database
		if d, err = s.GetDatabase(ctx(), app.LinkedDB); err == nil {
			err = n.ReplaceDatabase(ctx(), dbSpec(d), current)
		}
	}
	if err != nil {
		keep := n.SetAside(app.Name, current)
		fmt.Fprintf(out, "ERROR: couldn't put the current data back (%v); it's saved in %s\n", err, keep)
		return false
	}
	fmt.Fprintln(out, "put the current data back")
	startApp(s, app, out)
	restartWorker(s, app, out)
	return true
}

// stopApp stops the app and its worker so nothing writes to the database.
func stopApp(s *store.Store, app store.App) error {
	return AppNode(s, app).StopApp(ctx(), liveSpec(app))
}

// startApp starts the app's stopped live container again.
func startApp(s *store.Store, app store.App, out io.Writer) {
	AppNode(s, app).StartApp(ctx(), liveSpec(app), out)
}

func restartWorker(s *store.Store, app store.App, out io.Writer) {
	if w, err := s.GetWorker(ctx(), app.Name); err == nil {
		if err := runWorker(s, app, w, out); err != nil {
			fmt.Fprintln(out, "failed to start the worker again:", err)
		}
	}
}

// liveSpec is what the node needs of the app to find and reach its live
// container, without the variables a new container gets.
// buildPrefixes name the variables a build gets: Railpack's own settings
// (RAILPACK_START_CMD, RAILPACK_NODE_VERSION, …), and what frontend tools
// put in the JavaScript they build (VITE_API_URL, NEXT_PUBLIC_…), public
// by design. No other variable reaches the build: what a build sees can
// end up in the image's layers.
var buildPrefixes = []string{"RAILPACK_", "VITE_", "NEXT_PUBLIC_", "PUBLIC_", "REACT_APP_", "NUXT_PUBLIC_", "EXPO_PUBLIC_"}

// buildEnv are the app's variables its build gets, its project's shared
// ones included, references filled in.
func buildEnv(s *store.Store, app store.App) ([]string, error) {
	env, err := appEnv(s, app, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, kv := range env {
		for _, prefix := range buildPrefixes {
			if strings.HasPrefix(kv, prefix) {
				out = append(out, kv)
				break
			}
		}
	}
	return out, nil
}

func liveSpec(app store.App) node.AppSpec {
	return node.AppSpec{
		Name: app.Name, Project: app.ProjectName, ActiveSlot: app.ActiveSlot, ProxyPort: app.Port,
		ContainerPort: app.ContainerPort, PortHint: portHint(app, 0), HealthPath: app.HealthCheckPath,
		MemoryMB: app.MemoryMB, CPUs: app.Cpus, Command: app.StartCommand,
	}
}

// appSpec is liveSpec with what a new container of the app gets: its
// variables, told to listen on hint, and its volumes.
func appSpec(s *store.Store, app store.App, hint int64) (node.AppSpec, error) {
	spec := liveSpec(app)
	spec.PortHint = hint
	var err error
	if spec.Env, err = appEnv(s, app, hint); err != nil {
		return spec, err
	}
	if spec.Binds, err = appBinds(s, app.Name); err != nil {
		return spec, err
	}
	spec.Recreate = len(spec.Binds) > 0 && app.ShareVolumes == 0
	return spec, nil
}

// rollOut is the blue/green swap of the app to img on its node: start the
// inactive slot and health-check it, record it as live, point the proxy
// at it, then remove the old slot. If the candidate never gets healthy
// the old slot keeps serving.
//
// An app with volumes is recreated instead, unless it shares them: the old
// version is stopped first so two versions never write the same data, and
// started again if the new one fails.
func rollOut(s *store.Store, app store.App, img node.Image, out io.Writer) error {
	n := AppNode(s, app)
	spec, err := appSpec(s, app, portHint(app, n.ExposedPort(ctx(), app.Name, img)))
	if err != nil {
		return err
	}
	if err := ensureProjectNetworks(s, app.ProjectName); err != nil {
		return err
	}
	c, err := n.StartCandidate(ctx(), spec, img, out)
	if err != nil {
		return err
	}
	// The record first: after a crash between the two, the agent points
	// the proxy at the slot the record names (EnsureProxy), and
	// ReconcileSlots removes the other one.
	if err := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: app.Name, ActiveSlot: c.Slot, LivePort: c.Port}); err != nil {
		return n.Discard(ctx(), spec, c, out, err)
	}
	if err := n.Switch(ctx(), spec, c); err != nil {
		if rerr := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: app.Name, ActiveSlot: app.ActiveSlot, LivePort: app.LivePort}); rerr != nil {
			fmt.Fprintln(out, "warning:", rerr)
		}
		return n.Discard(ctx(), spec, c, out, err)
	}
	if err := SyncTunnel(s); err != nil {
		fmt.Fprintln(out, "warning: failed to update the tunnel's routes, retrying in the background:", err)
	}
	n.Retire(ctx(), spec, c, out)
	fmt.Fprintf(out, "live: container port %d, 127.0.0.1:%d (%s)\n", c.Port, app.Port, node.Tag(app.Name, img))
	return nil
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

// EnsureProxy restarts the proxy of an app on the panel's server after an
// agent restart and points it at the live container; a server that joined
// does its own as it connects.
func EnsureProxy(s *store.Store, app store.App) {
	if onLocal(s, app) {
		local.EnsureProxy(ctx(), liveSpec(app))
	}
}

func RemoveProxy(s *store.Store, app store.App) { AppNode(s, app).RemoveProxy(app.Name) }

// ReconcileSlots tidies up after jobs the agent didn't live to finish on
// the panel's server, run at startup before any job (see
// node.Local.Reconcile).
func ReconcileSlots(s *store.Store) {
	apps, err := s.ListApps(ctx())
	if err != nil {
		panellog.Error("failed to check the apps' containers:", err)
		return
	}
	// Servers that joined are reconciled as they connect (serverConnected).
	var specs []node.AppSpec
	for _, app := range apps {
		if !IsDeploying(app.Name) && onLocal(s, app) {
			specs = append(specs, liveSpec(app))
		}
	}
	local.Reconcile(ctx(), specs)
}

// Workers.

func runWorker(s *store.Store, app store.App, w store.Worker, out io.Writer) error {
	if err := ensureProjectNetworks(s, app.ProjectName); err != nil {
		return err
	}
	spec := liveSpec(app)
	var err error
	if spec.Env, err = WorkerEnv(s, app, w); err != nil {
		return err
	}
	if spec.Binds, err = appBinds(s, app.Name); err != nil {
		return err
	}
	return AppNode(s, app).RunWorker(ctx(), node.WorkerSpec{App: spec, Command: string(w.Command)}, out)
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
	if ok, _ := AppNode(s, app).HasImage(ctx(), app.Name, node.Latest); !ok {
		return nil // started by the first successful deploy
	}
	return runWorker(s, app, w, io.Discard)
}

func DeleteWorker(s *store.Store, appName string) error {
	app, err := s.GetApp(ctx(), appName)
	if err != nil {
		return err
	}
	if err := AppNode(s, app).RemoveWorker(ctx(), appName); err != nil {
		return err
	}
	if err := s.DeleteSealedVarsOf(ctx(), store.DeleteSealedVarsOfParams{Scope: "worker", Owner: appName}); err != nil {
		return err
	}
	return s.DeleteWorker(ctx(), appName)
}
