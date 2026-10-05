// Package nodetest is a node.Node in memory, for testing the panel's side
// without Docker: a deploy, rollback or backup runs in milliseconds, and
// any operation can be made to fail, as a node that is another server
// would when it drops off mid-job.
package nodetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/x0ryz/hakobu/internal/node"
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

	Stack      string                           // what Build detects
	Port       int64                            // a candidate answers on it (default 8080)
	Containers map[string]string                // name → status
	Runs       map[string]string                // container → build
	Images     map[string]map[node.Image]string // app → image → build
	Proxies    map[string]string                // app → container
	DBs        map[string]string                // database → content
	Passwords  map[string]string                // role → password
	Dumps      map[node.Dump]string             // dump → content
	Volumes    map[string]string                // node.Volume name → content
	Bucket     map[string]string                // backups uploaded: the first part's URL path → content
	Tunnels    map[string]node.TunnelSpec       // container → spec
	Edges      map[string][]string              // container → projects whose edge network it's on
	Networks   map[string]bool                  // projects with networks
}

var _ node.Node = (*Fake)(nil)

func New() *Fake {
	return &Fake{
		fail: map[string]error{}, hooks: map[string]func(){}, Port: 8080,
		Containers: map[string]string{}, Runs: map[string]string{}, Images: map[string]map[node.Image]string{},
		Proxies: map[string]string{}, DBs: map[string]string{}, Passwords: map[string]string{},
		Dumps: map[node.Dump]string{}, Volumes: map[string]string{}, Bucket: map[string]string{}, Tunnels: map[string]node.TunnelSpec{},
		Edges: map[string][]string{}, Networks: map[string]bool{},
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

func (f *Fake) State(ctx context.Context, container string) node.State {
	unlock, _ := f.call("State")
	defer unlock()
	if st, ok := f.Containers[container]; ok {
		return node.State{Status: st}
	}
	return node.State{Status: "not found"}
}

func (f *Fake) Statuses(ctx context.Context) (map[string]string, error) {
	unlock, err := f.call("Statuses")
	defer unlock()
	return clone(f.Containers), err
}

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

func (f *Fake) Build(ctx context.Context, b node.BuildSpec, out io.Writer) (string, error) {
	unlock, err := f.call("Build")
	defer unlock()
	if err != nil {
		return "", err
	}
	f.image(b.App)[node.Next] = f.next("build-")
	return f.Stack, nil
}

func (f *Fake) ExposedPort(ctx context.Context, app string, img node.Image) int64 { return 0 }

func (f *Fake) HasImage(ctx context.Context, app string, img node.Image) (bool, error) {
	unlock, err := f.call("HasImage")
	defer unlock()
	return f.Images[app][img] != "", err
}

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

func (f *Fake) SwapForRollback(ctx context.Context, app string) error {
	unlock, err := f.call("SwapForRollback")
	defer unlock()
	if err != nil {
		return err
	}
	img := f.image(app)
	img[node.Latest], img[node.Previous] = img[node.Previous], img[node.Latest]
	return nil
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
	delete(f.Dumps, node.SnapshotOf(app))
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

func (f *Fake) SetPassword(ctx context.Context, d node.DBSpec) error {
	unlock, err := f.call("SetPassword")
	defer unlock()
	if err == nil {
		f.Passwords[d.User] = d.Password
	}
	return err
}

// Backups: the content goes to Bucket, unsealed, under the path of its
// first part's URL; a download checks it against its SHA-256.

func (f *Fake) upload(up node.Upload, content string) (node.Uploaded, error) {
	if up.FileKey == "" {
		return node.Uploaded{}, errors.New("no key to seal the backup with")
	}
	u, err := up.URL(0)
	if err != nil {
		return node.Uploaded{}, err
	}
	f.Bucket[objectPath(u)] = content
	sum := sha256.Sum256([]byte(content))
	return node.Uploaded{Parts: 1, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (f *Fake) download(dl node.Download) (string, error) {
	u, err := dl.URL(0)
	if err != nil {
		return "", err
	}
	content, ok := f.Bucket[objectPath(u)]
	if !ok {
		return "", fmt.Errorf("no backup at %s", objectPath(u))
	}
	if sum := sha256.Sum256([]byte(content)); hex.EncodeToString(sum[:]) != dl.SHA256 {
		return "", errors.New("the backup in the bucket doesn't match the one hakobu made (SHA-256 differs), not restoring it")
	}
	return content, nil
}

func objectPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Path
}

func (f *Fake) BackupDatabase(ctx context.Context, d node.DBSpec, up node.Upload) (node.Uploaded, error) {
	unlock, err := f.call("BackupDatabase")
	defer unlock()
	if err != nil {
		return node.Uploaded{}, err
	}
	return f.upload(up, f.DBs[d.Name])
}

func (f *Fake) RestoreDatabase(ctx context.Context, d node.DBSpec, dl node.Download) error {
	unlock, err := f.call("RestoreDatabase")
	defer unlock()
	if err != nil {
		return err
	}
	content, err := f.download(dl)
	if err == nil {
		f.DBs[d.Name] = content
	}
	return err
}

// VerifyDatabaseBackup counts a non-empty dump as one table.
func (f *Fake) VerifyDatabaseBackup(ctx context.Context, d node.DBSpec, dl node.Download) (int, error) {
	unlock, err := f.call("VerifyDatabaseBackup")
	defer unlock()
	if err != nil {
		return 0, err
	}
	content, err := f.download(dl)
	if err != nil || content == "" {
		return 0, err
	}
	return 1, nil
}

func (f *Fake) BackupVolume(ctx context.Context, app, name string, up node.Upload) (node.Uploaded, error) {
	unlock, err := f.call("BackupVolume")
	defer unlock()
	if err != nil {
		return node.Uploaded{}, err
	}
	return f.upload(up, f.Volumes[node.Volume(app, name)])
}

// RestoreVolume checks the backup before it stops anything, as Local.
func (f *Fake) RestoreVolume(ctx context.Context, app node.AppSpec, name string, dl node.Download, out io.Writer) error {
	unlock, err := f.call("RestoreVolume")
	defer unlock()
	if err != nil {
		return err
	}
	content, err := f.download(dl)
	if err != nil {
		return err
	}
	f.Volumes[node.Volume(app.Name, name)] = content
	return nil
}

// PutVolumeFile keeps the file's content at "<volume>/<name>" in Volumes.
// The panel uploads the file to the bucket itself, sealed, so it's
// fetched as Local fetches it.
func (f *Fake) PutVolumeFile(ctx context.Context, app node.AppSpec, volume, name string, dl node.Download, out io.Writer) error {
	unlock, err := f.call("PutVolumeFile")
	defer unlock()
	if err != nil {
		return err
	}
	body, err := node.Fetch(ctx, dl)
	if err != nil {
		return err
	}
	defer body.Close()
	content, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.Volumes[node.Volume(app.Name, volume)+"/"+name] = string(content)
	return nil
}

// VerifyVolumeBackup counts a non-empty archive as one file.
func (f *Fake) VerifyVolumeBackup(ctx context.Context, dl node.Download) (int, error) {
	unlock, err := f.call("VerifyVolumeBackup")
	defer unlock()
	if err != nil {
		return 0, err
	}
	content, err := f.download(dl)
	if err != nil || content == "" {
		return 0, err
	}
	return 1, nil
}

// Snapshots.

func (f *Fake) SaveDump(ctx context.Context, d node.DBSpec) (node.Dump, error) {
	unlock, err := f.call("SaveDump")
	defer unlock()
	if err != nil {
		return "", err
	}
	dump := node.Dump(f.next("dump-") + ".tmp")
	f.Dumps[dump] = f.DBs[d.Name]
	return dump, nil
}

func (f *Fake) DropDump(dump node.Dump) {
	unlock, _ := f.call("DropDump")
	defer unlock()
	delete(f.Dumps, dump)
}

func (f *Fake) KeepSnapshot(app string, dump node.Dump) error {
	unlock, err := f.call("KeepSnapshot")
	defer unlock()
	if err != nil {
		return err
	}
	if dump == "" {
		delete(f.Dumps, node.SnapshotOf(app))
		return nil
	}
	content, ok := f.Dumps[dump]
	if !ok {
		return fmt.Errorf("no dump %s", dump)
	}
	delete(f.Dumps, dump)
	f.Dumps[node.SnapshotOf(app)] = content
	return nil
}

func (f *Fake) HasSnapshot(app string) bool {
	unlock, _ := f.call("HasSnapshot")
	defer unlock()
	_, ok := f.Dumps[node.SnapshotOf(app)]
	return ok
}

func (f *Fake) SetAside(app string, dump node.Dump) string {
	unlock, _ := f.call("SetAside")
	defer unlock()
	aside := node.Dump(app + "-before-rollback")
	f.Dumps[aside] = f.Dumps[dump]
	delete(f.Dumps, dump)
	return string(aside)
}

func (f *Fake) PruneSnapshots(apps map[string]bool) {
	unlock, _ := f.call("PruneSnapshots")
	defer unlock()
	for dump := range f.Dumps {
		if app, ok := strings.CutSuffix(string(dump), string(node.SnapshotOf(""))); ok && !apps[app] && !strings.HasSuffix(app, "-before-rollback") {
			delete(f.Dumps, dump)
		}
	}
}

func (f *Fake) ReplaceDatabase(ctx context.Context, d node.DBSpec, dump node.Dump) error {
	unlock, err := f.call("ReplaceDatabase")
	defer unlock()
	if err != nil {
		return err
	}
	content, ok := f.Dumps[dump]
	if !ok {
		return fmt.Errorf("no dump %s", dump)
	}
	f.DBs[d.Name] = content
	return nil
}

// Volumes.

func (f *Fake) HasVolume(ctx context.Context, app, name string) (bool, error) {
	unlock, err := f.call("HasVolume")
	defer unlock()
	_, ok := f.Volumes[node.Volume(app, name)]
	return ok, err
}

func (f *Fake) RemoveVolume(ctx context.Context, app, name string) error {
	unlock, err := f.call("RemoveVolume")
	defer unlock()
	delete(f.Volumes, node.Volume(app, name))
	return err
}

func (f *Fake) RemoveVolumesExcept(ctx context.Context, keep map[string]bool) error {
	unlock, err := f.call("RemoveVolumesExcept")
	defer unlock()
	for v := range f.Volumes {
		if !keep[v] {
			delete(f.Volumes, v)
		}
	}
	return err
}

// The machine.

func (f *Fake) Readings(ctx context.Context, containers map[string]bool) (node.Readings, error) {
	unlock, err := f.call("Readings")
	defer unlock()
	return node.Readings{}, err
}

func (f *Fake) Disk(ctx context.Context) (uint64, uint64, error) {
	unlock, err := f.call("Disk")
	defer unlock()
	return 0, 0, err
}

func (f *Fake) Cleanup(ctx context.Context, c node.CleanupSpec) (int64, int, error) {
	unlock, err := f.call("Cleanup")
	defer unlock()
	return 0, 0, err
}

// WatchDeaths reports nothing until ctx ends.
func (f *Fake) WatchDeaths(ctx context.Context, fn func(container, app string, d node.Death)) error {
	unlock, err := f.call("WatchDeaths")
	unlock()
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
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

func (f *Fake) EnsureProjectNetworks(ctx context.Context, project string, tunnels []node.TunnelSpec) error {
	unlock, err := f.call("EnsureProjectNetworks")
	defer unlock()
	if err != nil {
		return err
	}
	f.Networks[project] = true
	for _, t := range tunnels {
		edges := slices.DeleteFunc(f.Edges[t.Container()], func(p string) bool { return p == project })
		if slices.Contains(t.Projects, project) {
			edges = append(edges, project)
		}
		f.Edges[t.Container()] = edges
	}
	return nil
}

func (f *Fake) RemoveProjectNetworks(ctx context.Context, project string, tunnels []node.TunnelSpec) error {
	unlock, err := f.call("RemoveProjectNetworks")
	defer unlock()
	if err != nil {
		return err
	}
	delete(f.Networks, project)
	for c, edges := range f.Edges {
		f.Edges[c] = slices.DeleteFunc(edges, func(p string) bool { return p == project })
	}
	return nil
}

func (f *Fake) RunTunnel(ctx context.Context, t node.TunnelSpec) error {
	unlock, err := f.call("RunTunnel")
	defer unlock()
	if err == nil && t.Token != "" {
		f.Tunnels[t.Container()] = t
		f.Containers[t.Container()] = "running"
		if _, ok := f.Edges[t.Container()]; !ok {
			f.Edges[t.Container()] = slices.Clone(t.Projects)
		}
	}
	return err
}

func (f *Fake) RemoveTunnel(ctx context.Context, client string) error {
	unlock, err := f.call("RemoveTunnel")
	defer unlock()
	name := node.TunnelName(client)
	delete(f.Tunnels, name)
	delete(f.Containers, name)
	delete(f.Edges, name)
	return err
}

func (f *Fake) StartIngestRelay(ctx context.Context, socket string) error {
	unlock, err := f.call("StartIngestRelay")
	defer unlock()
	return err
}
