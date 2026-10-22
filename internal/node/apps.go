package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/proxy"
)

// ProjectNetwork is the project's own network: its apps, workers and
// caches, and the shared Postgres (whose databases have their own
// credentials). An app can't reach another project's. Project names have no
// "_", so no two projects' networks clash.
func ProjectNetwork(project string) string { return "hakobu_" + project }

// Image is one of an app's builds: the one being rolled out, the live one,
// and the one before it, which Rollback returns to.
type Image string

const (
	Next     Image = "next"
	Latest   Image = "latest"
	Previous Image = "previous"
)

// Tag is the Docker tag of the app's image.
func Tag(app string, img Image) string { return "hakobu/" + app + ":" + string(img) }

// AppSpec is what the machine needs to know of an app to run it, its
// variables and secrets resolved.
type AppSpec struct {
	Name          string
	Project       string
	ActiveSlot    string // "blue" or "green", the live one ("" before the first deploy)
	ProxyPort     int64  // the app's proxy on the node's 127.0.0.1
	ContainerPort int64  // 0: detect the port it listens on
	PortHint      int64  // the PORT it's told to listen on
	HealthPath    string
	MemoryMB      int64
	CPUs          float64
	Env           []string
	Binds         []string
	Command       string // run instead of the image's start, "" for its own
	// Recreate stops the live version before the new one starts, for an
	// app whose volumes mustn't be written by two versions at once.
	Recreate bool
	// Stopped: its owner stopped it, so nothing starts it again but a
	// deploy (which clears this) or Start.
	Stopped bool
}

// Live is the app's live container.
func (a AppSpec) Live() string {
	slot := a.ActiveSlot
	if slot == "" {
		slot = "blue"
	}
	return a.Name + "-" + slot
}

func (a AppSpec) options() deploy.AppOptions {
	return deploy.AppOptions{App: a.Name, Network: ProjectNetwork(a.Project), Env: a.Env, Binds: a.Binds, MemoryMB: a.MemoryMB, CPUs: a.CPUs, Command: a.Command}
}

// Candidate is a new version started next to the live one and healthy,
// not serving yet.
type Candidate struct {
	Slot      string // its slot, live once switched
	Container string
	Old       string // the live version's container
	Port      int64  // the container port it answers on
	IP        string // on the project's network
}

// ExposedPort is the port the image's EXPOSE names, 0 if none.
func (Local) ExposedPort(ctx context.Context, app string, img Image) int64 {
	return int64(deploy.ExposedPort(ctx, Tag(app, img)))
}

// DropImage deletes the app's build; one that isn't there is no error.
func (Local) DropImage(ctx context.Context, app string, img Image) {
	_ = deploy.RemoveImage(ctx, Tag(app, img))
}

// ErrPreviousNotKept: the build Promote replaced couldn't become Previous.
var ErrPreviousNotKept = errors.New("failed to keep the previous image for rollback")

// Promote makes the Next build that just went live Latest; the image it
// replaced becomes Previous and the old Previous is deleted. It returns
// ErrPreviousNotKept, once Latest is the new build, if the replaced image
// couldn't become Previous.
func (Local) Promote(ctx context.Context, app string) error {
	next, latest, prev := Tag(app, Next), Tag(app, Latest), Tag(app, Previous)
	dropped := deploy.ImageID(ctx, prev)
	var kept error
	if ok, err := deploy.ImageExists(ctx, latest); err != nil {
		kept = fmt.Errorf("%w: %v", ErrPreviousNotKept, err)
	} else if ok {
		if err := deploy.TagImage(ctx, latest, prev); err != nil {
			kept = fmt.Errorf("%w: %v", ErrPreviousNotKept, err)
		}
	}
	if err := deploy.TagImage(ctx, next, latest); err != nil {
		return err
	}
	if dropped != "" && dropped != deploy.ImageID(ctx, prev) && dropped != deploy.ImageID(ctx, latest) {
		_ = deploy.RemoveImage(ctx, dropped)
	}
	return kept
}

// CandidateWait is how long a new version gets to answer its health check;
// tests, whose apps answer at once, wait less.
var CandidateWait = time.Minute

// StartCandidate starts img in the app's inactive slot and waits up to
// CandidateWait for it to answer its health check on the project's network.
// A candidate that doesn't is removed, and the live version keeps serving
// (or, for Recreate, is started again). Switch then points the proxy at a
// healthy one, or Discard drops it.
func (n Local) StartCandidate(ctx context.Context, app AppSpec, img Image, out io.Writer) (Candidate, error) {
	oldSlot, newSlot := "blue", "green"
	if app.ActiveSlot == "green" {
		oldSlot, newSlot = "green", "blue"
	}
	c := Candidate{Slot: newSlot, Container: app.Name + "-" + newSlot, Old: app.Name + "-" + oldSlot}
	if app.Recreate {
		fmt.Fprintln(out, "the app has volumes: stopping", c.Old, "before starting the new version")
		if err := deploy.StopContainer(ctx, c.Old); err != nil {
			return c, err
		}
	}
	restoreOld := func() {
		if app.Recreate {
			n.restart(ctx, app, c.Old, out)
		}
	}

	fmt.Fprintln(out, "starting", c.Container)
	if _, err := deploy.RunAppContainer(ctx, Tag(app.Name, img), c.Container, app.options()); err != nil {
		restoreOld()
		return c, err
	}
	ip, err := deploy.ContainerIP(ctx, c.Container, ProjectNetwork(app.Project))
	if err != nil {
		_ = deploy.RemoveContainer(ctx, c.Container)
		restoreOld()
		return c, err
	}
	c.IP = ip
	path := "/" + strings.TrimPrefix(app.HealthPath, "/")
	requireOK := path != "/"
	if app.ContainerPort > 0 {
		fmt.Fprintf(out, "waiting for port %d to answer %s...\n", app.ContainerPort, path)
	} else {
		fmt.Fprintf(out, "waiting for the app to listen on a port (PORT=%d) and answer %s...\n", app.PortHint, path)
	}

	var loopbackOnly []int
	var crashed, oom, killed bool
	healthy := deploy.WaitHealthy(int(CandidateWait/time.Second), time.Second, func() bool {
		// A container that exits or restarts will never answer; stop waiting.
		if st := deploy.ContainerState(ctx, c.Container); st.Status == "exited" || st.Status == "dead" || st.Restarts > 0 || st.OOMKilled {
			st = awaitOOMFlag(ctx, app, c.Container, st)
			crashed, oom, killed = true, st.OOMKilled, st.ExitCode == 137
			return true
		}
		c.Port = app.ContainerPort
		if c.Port == 0 {
			var reachable []int
			reachable, loopbackOnly, _ = deploy.ListeningPorts(ctx, c.Container)
			c.Port = pickPort(reachable, app.PortHint)
		}
		return c.Port > 0 && deploy.HTTPCheck(fmt.Sprintf("http://%s:%d%s", ip, c.Port, path), requireOK)
	})
	if !healthy || crashed {
		logs, _ := deploy.ContainerLogs(ctx, c.Container, 50)
		_ = deploy.RemoveContainer(ctx, c.Container)
		restoreOld()
		within := fmt.Sprintf("%ds", int(CandidateWait.Seconds()))
		reason := fmt.Sprintf("didn't answer on port %d within %s", c.Port, within)
		switch {
		case oom:
			reason = "was killed: " + OOMText(app.MemoryMB)
		case killed && app.MemoryMB > 0:
			reason = "was killed (exit 137), most likely because " + OOMText(app.MemoryMB)
		case crashed:
			reason = "exited while starting (see its output below; a missing variable or an unreachable database are the usual causes)"
		case c.Port == 0 && len(loopbackOnly) > 0:
			reason = fmt.Sprintf("listens only on 127.0.0.1 (port %v); bind it to 0.0.0.0", loopbackOnly)
		case c.Port == 0:
			reason = "didn't listen on any port within " + within
		case requireOK:
			reason = fmt.Sprintf("didn't return 2xx on port %d%s within %s", c.Port, path, within)
		}
		kept := "previous version keeps running"
		if app.Recreate {
			kept = "previous version was started again"
		}
		return c, fmt.Errorf("new version %s; %s\n--- container output ---\n%s", reason, kept, logs)
	}

	// Healthy: keep it running. Until the proxy points at it, any failure
	// removes it and leaves the old version serving.
	if err := deploy.KeepRestarting(ctx, c.Container); err != nil {
		return c, n.Discard(ctx, app, c, out, err)
	}
	if _, err := proxy.Ensure(app.Name, app.ProxyPort); err != nil {
		return c, n.Discard(ctx, app, c, out, err)
	}
	return c, nil
}

// Discard removes a candidate that won't go live and starts the old
// version again if Recreate stopped it; it returns err.
func (n Local) Discard(ctx context.Context, app AppSpec, c Candidate, out io.Writer, err error) error {
	if rerr := deploy.RemoveContainer(ctx, c.Container); rerr != nil {
		fmt.Fprintln(out, "warning:", rerr)
	}
	if app.Recreate {
		n.restart(ctx, app, c.Old, out)
	}
	return err
}

// Switch points the app's proxy at the candidate. The state names the
// candidate's slot as live first: after a crash in between, the proxy
// follows the state (EnsureProxy) and Reconcile removes the other.
func (Local) Switch(ctx context.Context, app AppSpec, c Candidate) error {
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", c.IP, c.Port))
	return proxy.SetTarget(app.Name, u)
}

// Retire gives the old version up to 10 seconds to finish the requests in
// flight, the proxy sending new ones to the new version already, and
// removes it.
func (Local) Retire(ctx context.Context, app AppSpec, c Candidate, out io.Writer) {
	if err := deploy.StopContainer(ctx, c.Old); err != nil {
		fmt.Fprintln(out, "warning:", err) // removing it below kills it anyway
	}
	if err := deploy.RemoveContainer(ctx, c.Old); err != nil {
		fmt.Fprintln(out, "warning: failed to remove previous container:", err)
	}
}

// awaitOOMFlag rereads a stopped container's state for up to a second when
// it may have run out of memory but isn't flagged yet: Docker can report the
// exit before the OOM kill (seen under rootless Docker). Only a container
// with a memory limit gets the flag, so others aren't kept waiting.
func awaitOOMFlag(ctx context.Context, app AppSpec, container string, st State) State {
	for i := 0; i < 5 && !st.OOMKilled && app.MemoryMB > 0; i++ {
		time.Sleep(200 * time.Millisecond)
		st = deploy.ContainerState(ctx, container)
	}
	return st
}

// OOMText says why a container with that memory limit (0: none) was
// killed for memory.
func OOMText(memoryMB int64) string {
	if memoryMB > 0 {
		return fmt.Sprintf("it used more than its %d MB memory limit", memoryMB)
	}
	return "the server ran out of memory"
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

// StopApp stops the app's live container and its worker, so nothing
// writes to its database or volumes.
func (Local) StopApp(ctx context.Context, app AppSpec) error {
	if err := deploy.StopContainer(ctx, app.Live()); err != nil {
		return fmt.Errorf("couldn't stop %s: %w", app.Name, err)
	}
	if err := deploy.StopContainer(ctx, app.Name+"-worker"); err != nil {
		return fmt.Errorf("couldn't stop the worker of %s: %w", app.Name, err)
	}
	return nil
}

// StartApp starts the app's stopped live container and its worker again,
// and points the proxy at the app.
func (n Local) StartApp(ctx context.Context, app AppSpec, out io.Writer) {
	if app.Stopped {
		return // stopped by its owner before the restore that paused it
	}
	n.restart(ctx, app, app.Live(), out)
	worker := WorkerContainer(app.Name)
	if st, _ := deploy.ContainerStatus(ctx, worker); st == "exited" || st == "created" {
		if err := deploy.StartContainer(ctx, worker); err != nil {
			fmt.Fprintln(out, "failed to start the worker again:", err)
		}
	}
}

func (Local) restart(ctx context.Context, app AppSpec, name string, out io.Writer) {
	if status, _ := deploy.ContainerStatus(ctx, name); status == "not found" {
		return
	}
	if err := deploy.StartContainer(ctx, name); err != nil {
		fmt.Fprintln(out, "failed to start the previous version again:", err)
		return
	}
	ip, err := deploy.ContainerIP(ctx, name, ProjectNetwork(app.Project))
	if err != nil {
		fmt.Fprintln(out, "started the previous version again, but couldn't find its address:", err)
		return
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, app.PortHint))
	_ = proxy.SetTarget(app.Name, u)
	// Started isn't serving: wait until it answers again, so "the previous
	// version was started again" is true by the time it's said.
	path := "/" + strings.TrimPrefix(app.HealthPath, "/")
	if !deploy.WaitHealthy(int(CandidateWait/time.Second), time.Second, func() bool {
		return deploy.HTTPCheck(u.String()+path, path != "/")
	}) {
		fmt.Fprintln(out, "started the previous version again, but it doesn't answer", path, "yet:", name)
		return
	}
	fmt.Fprintln(out, "started the previous version again:", name)
}

// EnsureProxy starts the app's proxy, as hakobu starts, and points it at
// the live container.
func (Local) EnsureProxy(ctx context.Context, app AppSpec) {
	isNew, err := proxy.Ensure(app.Name, app.ProxyPort)
	if err != nil || !isNew {
		return
	}
	ip, err := deploy.ContainerIP(ctx, app.Live(), ProjectNetwork(app.Project))
	if err != nil {
		return
	}
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, app.PortHint))
	_ = proxy.SetTarget(app.Name, u)
}

func (Local) RemoveProxy(app string) { proxy.Remove(app) }

// Reconcile tidies up after a deploy hakobu didn't live to finish: the slot
// the state doesn't name is a leftover candidate or old version, and a
// stopped live container was stopped by a deploy of an app with volumes
// that never started it again. It runs as hakobu starts, before any deploy.
func (Local) Reconcile(ctx context.Context, apps []AppSpec) {
	for _, app := range apps {
		live := app.Live()
		status, _ := deploy.ContainerStatus(ctx, live)
		if status == "not found" || status == "unknown" {
			continue // never deployed, or Docker isn't answering: leave both alone
		}
		other := app.Name + "-green"
		if live == other {
			other = app.Name + "-blue"
		}
		if st, _ := deploy.ContainerStatus(ctx, other); st != "not found" && st != "unknown" {
			panellog.Info("removing", other+", left by an unfinished deploy of", app.Name)
			if err := deploy.RemoveContainer(ctx, other); err != nil {
				panellog.Error("failed to remove", other+":", err)
			}
		}
		if (status == "exited" || status == "created") && !app.Stopped {
			panellog.Info("starting", live+", stopped by an unfinished deploy")
			if err := deploy.StartContainer(ctx, live); err != nil {
				panellog.Error("failed to start", live+":", err)
			}
		}
		// A release command left running goes on to its end (a migration
		// cut short could leave the database half done); one that ended
		// is removed.
		release := app.Name + "-release"
		if st, _ := deploy.ContainerStatus(ctx, release); st == "exited" || st == "created" || st == "dead" {
			panellog.Info("removing", release+", left by an unfinished deploy")
			_ = deploy.RemoveContainer(ctx, release)
		}
	}
}

// WorkerSpec is the app's worker: its Latest image with another command.
type WorkerSpec struct {
	App     AppSpec // Env is the worker's
	Command string
}

func WorkerContainer(app string) string { return app + "-worker" }

// RunWorker (re)starts the app's worker.
func (Local) RunWorker(ctx context.Context, w WorkerSpec, out io.Writer) error {
	fmt.Fprintln(out, "starting worker", WorkerContainer(w.App.Name))
	_, err := deploy.RunWorkerContainer(ctx, Tag(w.App.Name, Latest), WorkerContainer(w.App.Name), w.Command, w.App.options())
	return err
}

func (Local) RemoveWorker(ctx context.Context, app string) error {
	return deploy.RemoveContainer(ctx, WorkerContainer(app))
}
