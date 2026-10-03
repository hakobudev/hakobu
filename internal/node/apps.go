package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x0ryz/hakobu/internal/build"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/proxy"
)

// Names on a node. Each project has its own networks, so an app can't
// reach the apps of other projects, private ones included:
//   - hakobu_<project>: its apps and workers, plus the shared Postgres
//     (whose databases have their own credentials);
//   - hakobu_<project>_edge: its live app containers under their aliases,
//     plus cloudflared, which reaches them there.
//
// A container on several networks doesn't route between them. Project
// names have no "_", so no two projects' network names can clash.

func ProjectNetwork(project string) string { return "hakobu_" + project }
func ProjectEdge(project string) string    { return "hakobu_" + project + "_edge" }

// EdgeAlias is the app's name on the edge network. App and container names
// have no dots, so it can't clash with a container name.
func EdgeAlias(app string) string { return app + ".hakobu" }

// WorkDir holds the app's clone while it builds.
func WorkDir(app string) string { return "data/work/" + app }

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

// AppSpec is what a node needs to know of an app to run it; the panel
// fills it from its records, variables and secrets resolved.
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
	// Recreate stops the live version before the new one starts, for an
	// app whose volumes mustn't be written by two versions at once.
	Recreate bool
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
	return deploy.AppOptions{App: a.Name, Network: ProjectNetwork(a.Project), Env: a.Env, Binds: a.Binds, MemoryMB: a.MemoryMB, CPUs: a.CPUs}
}

// BuildSpec is what to clone and how to build it.
type BuildSpec struct {
	App      string
	Repo     string // for the log
	CloneURL string
	Token    string // read access to the repo
	Path     string // the directory to build, inside the repo
	Strategy string // "railpack" or "dockerfile"
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

// Build clones the repo and builds it as the app's Next image, returning
// the stack it detected ("" if none).
func (Local) Build(ctx context.Context, b BuildSpec, out io.Writer) (stack string, err error) {
	dir := WorkDir(b.App)
	defer os.RemoveAll(dir) // the next deploy clones again anyway
	fmt.Fprintln(out, "cloning", b.Repo)
	if err := build.CloneRepo(b.CloneURL, b.Token, dir, out); err != nil {
		return "", fmt.Errorf("clone failed: %w", err)
	}
	src, err := buildDir(dir, b.Path)
	if err != nil {
		return "", err
	}
	stack = detect.StackOf(src)
	next := Tag(b.App, Next)
	fmt.Fprintf(out, "building %s from %s (%s)\n", next, src, b.Strategy)
	if err := build.BuildWithStrategy(src, next, b.Strategy, out); err != nil {
		return "", fmt.Errorf("build failed: %w", err)
	}
	return stack, nil
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

// ExposedPort is the port the image's EXPOSE names, 0 if none.
func (Local) ExposedPort(ctx context.Context, app string, img Image) int64 {
	return int64(deploy.ExposedPort(ctx, Tag(app, img)))
}

// HasImage reports whether the app has that build.
func (Local) HasImage(ctx context.Context, app string, img Image) (bool, error) {
	return deploy.ImageExists(ctx, Tag(app, img))
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

// SwapForRollback makes Previous, which just went live, Latest and the
// build it replaced Previous, so rolling back again returns to it.
func (Local) SwapForRollback(ctx context.Context, app string) error {
	prev, latest, swap := Tag(app, Previous), Tag(app, Latest), Tag(app, Next)
	defer func() { _ = deploy.RemoveImage(ctx, swap) }()
	for _, t := range [][2]string{{latest, swap}, {prev, latest}, {swap, prev}} {
		if err := deploy.TagImage(ctx, t[0], t[1]); err != nil {
			return err
		}
	}
	return nil
}

// CandidateWait is how long a new version gets to answer its health check;
// tests, whose apps answer at once, wait less.
var CandidateWait = time.Minute

// StartCandidate starts img in the app's inactive slot and waits up to
// CandidateWait for it to answer its health check on the project's network. A
// candidate that doesn't is removed, and the live version keeps serving
// (or, for Recreate, is started again). A healthy one joins the edge
// network under the app's alias next to the live version; Switch then
// points the proxy at it, or Discard drops it.
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

	// Healthy: keep it running, and let the tunnel find it under the app's
	// alias next to the old version. Until the proxy points at it, any
	// failure removes it and leaves the old version serving: two versions
	// left answering under the alias would split the traffic.
	if err := deploy.KeepRestarting(ctx, c.Container); err != nil {
		return c, n.Discard(ctx, app, c, out, err)
	}
	if err := deploy.ConnectNetwork(ctx, c.Container, ProjectEdge(app.Project), EdgeAlias(app.Name)); err != nil {
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

// Switch points the app's proxy at the candidate. The panel records the
// candidate's slot as live first: after a crash in between, the node's
// proxy follows the record (EnsureProxy) and Reconcile removes the other.
func (Local) Switch(ctx context.Context, app AppSpec, c Candidate) error {
	u, _ := url.Parse(fmt.Sprintf("http://%s:%d", c.IP, c.Port))
	return proxy.SetTarget(app.Name, u)
}

// Retire takes the old version off the edge network, so it gets no new
// requests, gives it up to 10 seconds to finish the ones in flight, and
// removes it.
func (Local) Retire(ctx context.Context, app AppSpec, c Candidate, out io.Writer) {
	if err := deploy.DisconnectNetwork(ctx, c.Old, ProjectEdge(app.Project)); err != nil {
		fmt.Fprintln(out, "warning:", err)
	}
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

// StartApp starts the app's stopped live container again and points the
// proxy at it.
func (n Local) StartApp(ctx context.Context, app AppSpec, out io.Writer) {
	n.restart(ctx, app, app.Live(), out)
}

func (Local) restart(ctx context.Context, app AppSpec, name string, out io.Writer) {
	if status, _ := deploy.ContainerStatus(ctx, name); status == "not found" {
		return
	}
	if err := deploy.StartContainer(ctx, name); err != nil {
		fmt.Fprintln(out, "failed to start the previous version again:", err)
		return
	}
	if ip, err := deploy.ContainerIP(ctx, name, ProjectNetwork(app.Project)); err == nil {
		u, _ := url.Parse(fmt.Sprintf("http://%s:%d", ip, app.PortHint))
		_ = proxy.SetTarget(app.Name, u)
	}
	fmt.Fprintln(out, "started the previous version again:", name)
}

// EnsureProxy starts the app's proxy, after an agent restart say, and
// points it at the live container.
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

// Reconcile tidies up after a job the node didn't live to finish: the slot
// the record doesn't name is a leftover candidate or old version (still
// answering under the app's edge alias), and a stopped live container was
// stopped by a recreate deploy or data rollback that never started it
// again. Apps with a job running mustn't be passed.
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
			fmt.Println("removing", other+", left by an unfinished deploy of", app.Name)
			if err := deploy.RemoveContainer(ctx, other); err != nil {
				fmt.Println("failed to remove", other+":", err)
			}
		}
		if status == "exited" || status == "created" {
			fmt.Println("starting", live+", stopped by an unfinished deploy")
			if err := deploy.StartContainer(ctx, live); err != nil {
				fmt.Println("failed to start", live+":", err)
			}
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
