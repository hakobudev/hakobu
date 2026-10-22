// Package nodetest is the machine in memory, for testing deploys without
// Docker: a deploy, rollback or backup runs in milliseconds, and any
// operation can be made to fail.
package nodetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/hakobudev/hakobu/internal/node"
)

// Fake is a node whose containers, builds, databases, dumps and volumes are
// maps. A container holds the build it runs; a database, dump or volume
// holds its content as a string.
type Fake struct {
	mu    sync.Mutex
	fail  map[string]error
	hooks map[string]func()
	calls []string
	n     int // for build, dump and container IDs

	Port       int64                            // a candidate answers on it (default 8080)
	Containers map[string]string                // name → status
	Runs       map[string]string                // container → build
	Images     map[string]map[node.Image]string // app → image → build
	Proxies    map[string]string                // app → container
	DBs        map[string]string                // database → content
	Passwords  map[string]string                // role → password
	Volumes    map[string]string                // node.Volume name → content
	Networks   map[string]bool                  // projects with networks
	Caches     map[string]node.CacheSpec        // container → spec
	Postgres   string                           // why Postgres doesn't answer, "" if it does
}

func New() *Fake {
	return &Fake{
		fail: map[string]error{}, hooks: map[string]func(){}, Port: 8080,
		Containers: map[string]string{}, Runs: map[string]string{}, Images: map[string]map[node.Image]string{},
		Proxies: map[string]string{}, DBs: map[string]string{}, Passwords: map[string]string{},
		Volumes: map[string]string{}, Networks: map[string]bool{}, Caches: map[string]node.CacheSpec{},
	}
}

// Fail makes every later call of method fail with err, until Fail(method,
// nil).
func (f *Fake) Fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, method)
	} else {
		f.fail[method] = err
	}
}

// OnCall runs fn when method is called, before it does its work: to drop
// the link mid-call, say.
func (f *Fake) OnCall(method string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks[method] = fn
}

// Calls are the methods called so far, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Called reports whether method was called.
func (f *Fake) Called(method string) bool { return slices.Contains(f.Calls(), method) }

// call notes a call and returns, with f locked until unlock, the error it
// is to fail with.
func (f *Fake) call(method string) (unlock func(), err error) {
	f.mu.Lock()
	if hook := f.hooks[method]; hook != nil {
		f.mu.Unlock()
		hook()
		f.mu.Lock()
	}
	f.calls = append(f.calls, method)
	return f.mu.Unlock, f.fail[method]
}

func (f *Fake) next(prefix string) string {
	f.n++
	return fmt.Sprintf("%s%d", prefix, f.n)
}

func (f *Fake) image(app string) map[node.Image]string {
	if f.Images[app] == nil {
		f.Images[app] = map[node.Image]string{}
	}
	return f.Images[app]
}

// Containers.

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (f *Fake) Logs(ctx context.Context, container string, lines int) (string, error) {
	unlock, err := f.call("Logs")
	defer unlock()
	return "", err
}

// Builds.

func (f *Fake) ExposedPort(ctx context.Context, app string, img node.Image) int64 { return 0 }

func (f *Fake) DropImage(ctx context.Context, app string, img node.Image) {
	unlock, _ := f.call("DropImage")
	defer unlock()
	delete(f.image(app), img)
}

// Promote fails with err before changing anything, except for
// node.ErrPreviousNotKept, which it returns once Next is Latest, as Local.
func (f *Fake) Promote(ctx context.Context, app string) error {
	unlock, err := f.call("Promote")
	defer unlock()
	img := f.image(app)
	if err != nil && !errors.Is(err, node.ErrPreviousNotKept) {
		return err
	}
	if img[node.Next] == "" {
		return fmt.Errorf("no build of %s to promote", app)
	}
	if err == nil && img[node.Latest] != "" {
		img[node.Previous] = img[node.Latest]
	}
	img[node.Latest] = img[node.Next]
	delete(img, node.Next)
	return err
}

// Rolling out.

func (f *Fake) StartCandidate(ctx context.Context, app node.AppSpec, img node.Image, out io.Writer) (node.Candidate, error) {
	unlock, err := f.call("StartCandidate")
	defer unlock()
	oldSlot, newSlot := "blue", "green"
	if app.ActiveSlot == "green" {
		oldSlot, newSlot = "green", "blue"
	}
	c := node.Candidate{Slot: newSlot, Container: app.Name + "-" + newSlot, Old: app.Name + "-" + oldSlot, Port: f.Port, IP: "10.0.0.2"}
	if err != nil {
		return c, err
	}
	if app.Recreate && f.Containers[c.Old] != "" {
		f.Containers[c.Old] = "exited"
	}
	f.Containers[c.Container] = "running"
	f.Runs[c.Container] = f.image(app.Name)[img]
	return c, nil
}

func (f *Fake) Switch(ctx context.Context, app node.AppSpec, c node.Candidate) error {
	unlock, err := f.call("Switch")
	defer unlock()
	if err == nil {
		f.Proxies[app.Name] = c.Container
	}
	return err
}

func (f *Fake) Retire(ctx context.Context, app node.AppSpec, c node.Candidate, out io.Writer) {
	unlock, _ := f.call("Retire")
	defer unlock()
	delete(f.Containers, c.Old)
	delete(f.Runs, c.Old)
}

func (f *Fake) Discard(ctx context.Context, app node.AppSpec, c node.Candidate, out io.Writer, err error) error {
	unlock, _ := f.call("Discard")
	defer unlock()
	delete(f.Containers, c.Container)
	delete(f.Runs, c.Container)
	if app.Recreate && f.Containers[c.Old] != "" {
		f.Containers[c.Old] = "running"
	}
	return err
}

// The live version.

func (f *Fake) StopApp(ctx context.Context, app node.AppSpec) error {
	unlock, err := f.call("StopApp")
	defer unlock()
	if err != nil {
		return err
	}
	for _, c := range []string{app.Live(), node.WorkerContainer(app.Name)} {
		if f.Containers[c] != "" {
			f.Containers[c] = "exited"
		}
	}
	return nil
}

func (f *Fake) StartApp(ctx context.Context, app node.AppSpec, out io.Writer) {
	unlock, _ := f.call("StartApp")
	defer unlock()
	if f.Containers[app.Live()] != "" {
		f.Containers[app.Live()] = "running"
		f.Proxies[app.Name] = app.Live()
	}
}

func (f *Fake) EnsureProxy(ctx context.Context, app node.AppSpec) {
	unlock, _ := f.call("EnsureProxy")
	defer unlock()
	if _, ok := f.Proxies[app.Name]; !ok {
		f.Proxies[app.Name] = app.Live()
	}
}

func (f *Fake) RemoveProxy(app string) {
	unlock, _ := f.call("RemoveProxy")
	defer unlock()
	delete(f.Proxies, app)
}

func (f *Fake) Reconcile(ctx context.Context, apps []node.AppSpec) {
	unlock, _ := f.call("Reconcile")
	defer unlock()
	for _, app := range apps {
		live := app.Live()
		if f.Containers[live] == "" {
			continue
		}
		other := app.Name + "-green"
		if live == other {
			other = app.Name + "-blue"
		}
		delete(f.Containers, other)
		delete(f.Runs, other)
		if f.Containers[live] == "exited" {
			f.Containers[live] = "running"
		}
	}
}

// Workers.

func (f *Fake) RunWorker(ctx context.Context, w node.WorkerSpec, out io.Writer) error {
	unlock, err := f.call("RunWorker")
	defer unlock()
	if err == nil {
		name := node.WorkerContainer(w.App.Name)
		f.Containers[name] = "running"
		f.Runs[name] = f.image(w.App.Name)[node.Latest]
	}
	return err
}

func (f *Fake) RemoveWorker(ctx context.Context, app string) error {
	unlock, err := f.call("RemoveWorker")
	defer unlock()
	delete(f.Containers, node.WorkerContainer(app))
	return err
}

func (f *Fake) RemoveApp(ctx context.Context, app string, volumes []string) error {
	unlock, err := f.call("RemoveApp")
	defer unlock()
	if err != nil {
		return err
	}
	for _, c := range []string{app + "-blue", app + "-green", node.WorkerContainer(app)} {
		delete(f.Containers, c)
		delete(f.Runs, c)
	}
	delete(f.Images, app)
	delete(f.Proxies, app)
	for _, v := range volumes {
		delete(f.Volumes, node.Volume(app, v))
	}
	return nil
}

// Databases.

func (f *Fake) EnsurePostgres(ctx context.Context) error {
	unlock, err := f.call("EnsurePostgres")
	defer unlock()
	return err
}

func (f *Fake) PostgresReady(ctx context.Context) bool {
	unlock, err := f.call("PostgresReady")
	defer unlock()
	return err == nil
}

func (f *Fake) CreateDatabase(ctx context.Context, d node.DBSpec) error {
	unlock, err := f.call("CreateDatabase")
	defer unlock()
	if err != nil {
		return err
	}
	if _, ok := f.DBs[d.Name]; ok {
		return fmt.Errorf("database %s exists", d.Name)
	}
	f.DBs[d.Name], f.Passwords[d.User] = "", d.Password
	return nil
}

func (f *Fake) EnsureDatabase(ctx context.Context, d node.DBSpec) error {
	unlock, err := f.call("EnsureDatabase")
	defer unlock()
	if _, ok := f.DBs[d.Name]; !ok && err == nil {
		f.DBs[d.Name], f.Passwords[d.User] = "", d.Password
	}
	return err
}

func (f *Fake) DropDatabase(ctx context.Context, d node.DBSpec) error {
	unlock, err := f.call("DropDatabase")
	defer unlock()
	if err == nil {
		delete(f.DBs, d.Name)
		delete(f.Passwords, d.User)
	}
	return err
}

func (f *Fake) EnsureCache(ctx context.Context, c node.CacheSpec) error {
	unlock, err := f.call("EnsureCache")
	defer unlock()
	if err == nil {
		f.Caches[c.Name] = c
		f.Containers[c.Name] = "running"
	}
	return err
}

func (f *Fake) DropCache(ctx context.Context, name string) error {
	unlock, err := f.call("DropCache")
	defer unlock()
	if err == nil {
		delete(f.Caches, name)
		delete(f.Containers, name)
	}
	return err
}

// Snapshots.

// DumpDatabase writes the database's content.
func (f *Fake) DumpDatabase(ctx context.Context, d node.DBSpec, w io.Writer) error {
	unlock, err := f.call("DumpDatabase")
	defer unlock()
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, f.DBs[d.Name])
	return err
}

// Volumes.

func (f *Fake) RemoveVolume(ctx context.Context, app, name string) error {
	unlock, err := f.call("RemoveVolume")
	defer unlock()
	delete(f.Volumes, node.Volume(app, name))
	return err
}

// The machine.

func (f *Fake) Disk(ctx context.Context) (uint64, uint64, error) {
	unlock, err := f.call("Disk")
	defer unlock()
	return 0, 0, err
}

func (f *Fake) CheckHealth(ctx context.Context, app node.AppSpec, port int64) (bool, string, bool) {
	unlock, err := f.call("CheckHealth")
	defer unlock()
	if f.Containers[app.Live()] != "running" {
		return false, "", false
	}
	if err != nil {
		return false, err.Error(), true
	}
	return true, "", true
}

// Networks.

func (f *Fake) EnsureProjectNetworks(ctx context.Context, project string) error {
	unlock, err := f.call("EnsureProjectNetworks")
	defer unlock()
	if err != nil {
		return err
	}
	f.Networks[project] = true
	return nil
}
