package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/spec"
)

// Release is one commit of a project as the server deploys it.
type Release struct {
	Spec *spec.Project
	// Images is the image each app runs, pinned by digest.
	Images map[string]string
	// Secrets are each app's secret variables, decrypted.
	Secrets map[string]map[string]string
	// Force redeploys every app, even with nothing changed: a restart.
	Force bool
	// Revision is the commit it was built from; an app it deploys gets it as HAKOBU_REVISION.
	Revision string
	// Pull is CI's token for its registry, if it sealed one in: it pulls
	// the apps when the server has no credentials of its own there.
	Pull *PullAuth
}

// Node is what apply needs of the machine; node.Local is the real one.
type Node interface {
	ExposedPort(ctx context.Context, app string, img node.Image) int64
	Promote(ctx context.Context, app string) error
	StartCandidate(ctx context.Context, app node.AppSpec, img node.Image, out io.Writer) (node.Candidate, error)
	Switch(ctx context.Context, app node.AppSpec, c node.Candidate) error
	Retire(ctx context.Context, app node.AppSpec, c node.Candidate, out io.Writer)
	Discard(ctx context.Context, app node.AppSpec, c node.Candidate, out io.Writer, err error) error
	RunWorker(ctx context.Context, w node.WorkerSpec, out io.Writer) error
	RemoveWorker(ctx context.Context, app string) error
	RemoveApp(ctx context.Context, app string, volumes []string) error
	EnsurePostgres(ctx context.Context) error
	EnsureDatabase(ctx context.Context, d node.DBSpec) error
	EnsureCache(ctx context.Context, c node.CacheSpec) error
	DumpDatabase(ctx context.Context, d node.DBSpec, w io.Writer) error
	EnsureProjectNetworks(ctx context.Context, project string) error
}

// Applier deploys releases on this server, one at a time.
type Applier struct {
	Node Node
	// Server is this server's name: a release for another is refused.
	Server    string
	StateFile string
	// Pull fetches an app's image and tags it as tag; nil pulls with
	// Docker, signed in with Auth's credentials for the image's host.
	Pull func(ctx context.Context, ref, tag string) error
	Auth map[string]deploy.RegistryAuth
	// RunOnce runs a release command; nil runs it with Docker.
	RunOnce func(ctx context.Context, tag, name, command string, opts deploy.AppOptions) (string, error)
	// ReleaseTimeout bounds a release command (default 30 minutes).
	ReleaseTimeout time.Duration
	// SnapshotDir keeps the dump of each database taken before a release
	// command, the last one of each: the way back if a migration goes
	// wrong. It's on the server's disk next to the database itself.
	SnapshotDir string

	// mu is held by whatever changes the state (a deploy, a destroy);
	// reads go without it (read).
	mu sync.Mutex
	// The ingress's routes, as of the state file's stamp (Domains).
	routesMu    sync.Mutex
	routes      map[string]int64
	routesStamp string
}

// FirstProxyPort is where apps' proxies on 127.0.0.1 start.
var FirstProxyPort int64 = 8081

// Names on the server. Apps and caches are containers named after their
// project; databases live in the shared Postgres, where - can't be bare.
func AppKey(project, app string) string { return project + "-" + app }
func CacheKey(project, c string) string { return project + "-" + c }
func DBKey(project, db string) string {
	return strings.ReplaceAll(project+"_"+db, "-", "_")
}

// Busy reports whether something changes the state now: a deploy, a
// destroy.
func (a *Applier) Busy() bool {
	if !a.mu.TryLock() {
		return true
	}
	a.mu.Unlock()
	return false
}

// Apply brings the server in line with r and returns what it changed. On
// an error the apps it didn't reach keep running as they were, and the
// state keeps what was made, so applying again picks up where it stopped.
func (a *Applier) Apply(ctx context.Context, r Release, out io.Writer) (Plan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Server != "" && r.Spec.Server != a.Server {
		return Plan{Project: r.Spec.Name}, fmt.Errorf("the release is for server %s, and this is %s", r.Spec.Server, a.Server)
	}
	st, err := LoadState(a.StateFile)
	if err != nil {
		return Plan{}, err
	}
	if err := domainsFree(st, r.Spec); err != nil {
		return Plan{Project: r.Spec.Name}, err
	}
	ps := st.Project(r.Spec.Name)
	plan, err := MakePlan(r.Spec, r.Images, ps)
	if err != nil {
		return plan, err
	}
	if r.Force {
		for _, n := range sortedKeys(r.Spec.Apps) {
			if !slices.Contains(plan.Redeploy, n) {
				plan.Changes = append(plan.Changes, Change{Update, "app", n, "redeployed (forced)"})
				plan.Redeploy = append(plan.Redeploy, n)
			}
		}
	}
	fmt.Fprint(out, plan)
	if plan.Empty() {
		return plan, nil
	}
	save := func() error { return st.Save(a.StateFile) }
	project := r.Spec.Name

	if len(r.Spec.Databases) > 0 {
		if err := a.Node.EnsurePostgres(ctx); err != nil {
			return plan, fmt.Errorf("postgres: %w", err)
		}
	}
	if err := a.Node.EnsureProjectNetworks(ctx, project); err != nil {
		return plan, err
	}
	for _, n := range sortedKeys(r.Spec.Databases) {
		d, err := a.database(ps, project, n, save)
		if err != nil {
			return plan, err
		}
		if err := a.Node.EnsureDatabase(ctx, d); err != nil {
			return plan, fmt.Errorf("databases.%s: %w", n, err)
		}
	}
	for _, n := range sortedKeys(r.Spec.Caches) {
		c, ok := ps.Caches[n]
		if !ok {
			c = &CacheState{Password: randomHex()}
			ps.Caches[n] = c
			if err := save(); err != nil {
				return plan, err
			}
		}
		spec := node.CacheSpec{Name: CacheKey(project, n), Project: project, Password: c.Password, MemoryMB: int64(r.Spec.Caches[n].Memory >> 20)}
		if err := a.Node.EnsureCache(ctx, spec); err != nil {
			return plan, fmt.Errorf("caches.%s: %w", n, err)
		}
	}
	for app, w := range r.Spec.Apps {
		for v := range w.Volumes {
			ps.Volumes[app+"/"+v] = true // Docker makes a named volume on first mount
		}
	}
	if err := save(); err != nil {
		return plan, err
	}

	redeployed := map[string]bool{}
	for _, n := range plan.Redeploy {
		if err := a.deployApp(ctx, r, st, ps, n, save, out); err != nil {
			return plan, fmt.Errorf("apps.%s: %w", n, err)
		}
		redeployed[n] = true
	}
	var last spec.Project
	if ps.Spec != nil {
		last = *ps.Spec
	}
	for _, n := range sortedKeys(r.Spec.Apps) {
		w := r.Spec.Apps[n]
		if redeployed[n] || w.Worker == last.Apps[n].Worker {
			continue
		}
		key := AppKey(project, n)
		if err := a.Node.RemoveWorker(ctx, key); err != nil {
			return plan, fmt.Errorf("apps.%s: %w", n, err)
		}
		if w.Worker != "" {
			if err := a.runWorker(ctx, r, ps, n, out); err != nil {
				return plan, fmt.Errorf("apps.%s: worker: %w", n, err)
			}
		}
	}
	for _, n := range sortedKeys(last.Apps) {
		if _, ok := r.Spec.Apps[n]; ok {
			continue
		}
		fmt.Fprintln(out, "removing app", n)
		if err := a.Node.RemoveApp(ctx, AppKey(project, n), nil); err != nil {
			return plan, fmt.Errorf("apps.%s: %w", n, err)
		}
		delete(ps.Apps, n)
		if err := save(); err != nil {
			return plan, err
		}
	}
	ps.Spec = r.Spec
	return plan, save()
}

// domainsFree refuses a domain another project on the server serves: the
// ingress could send its requests to either.
func domainsFree(st *State, want *spec.Project) error {
	var errs []error
	for _, other := range sortedKeys(st.Projects) {
		ps := st.Projects[other]
		if other == want.Name || ps.Spec == nil {
			continue
		}
		for _, n := range sortedKeys(want.Apps) {
			d := want.Apps[n].Domain
			for _, m := range sortedKeys(ps.Spec.Apps) {
				if d != "" && ps.Spec.Apps[m].Domain == d {
					errs = append(errs, fmt.Errorf("apps.%s.domain: %s is project %s's (apps.%s) on this server", n, d, other, m))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// database is d's spec, its password made and saved on first use, before
// Postgres has it: a crash in between leaves a password for no role,
// never a role nobody knows the password of.
func (a *Applier) database(ps *ProjectState, project, name string, save func() error) (node.DBSpec, error) {
	d, ok := ps.Databases[name]
	if !ok {
		d = &DatabaseState{Password: randomHex()}
		ps.Databases[name] = d
		if err := save(); err != nil {
			return node.DBSpec{}, err
		}
	}
	key := DBKey(project, name)
	return node.DBSpec{Name: key, User: key, Password: d.Password}, nil
}

// deployApp rolls an app out to its image: pull, release command, a new
// container next to the live one, switch once it's healthy, then its
// worker. Until the switch, any failure leaves the live version serving.
func (a *Applier) deployApp(ctx context.Context, r Release, st *State, ps *ProjectState, name string, save func() error, out io.Writer) error {
	project, w, ref := r.Spec.Name, r.Spec.Apps[name], r.Images[name]
	key := AppKey(project, name)
	as, ok := ps.Apps[name]
	if !ok {
		as = &AppState{ProxyPort: st.freePort()}
		ps.Apps[name] = as
		if err := save(); err != nil {
			return err
		}
	}
	if ref == "" {
		ref = as.Image // settings changed, same image
	}
	next := node.Tag(key, node.Next)
	switch id := a.localImage(ctx, ref, as); {
	case id != "":
		fmt.Fprintln(out, "using", ref, "from this server")
		if err := deploy.TagImage(ctx, id, next); err != nil {
			return err
		}
	default:
		fmt.Fprintln(out, "pulling", ref)
		if err := a.pull(ctx, r, ref, next); err != nil {
			return err
		}
	}
	imageID := a.imageID(ctx, next)
	app := a.appSpec(ctx, r, ps, name)
	app.ActiveSlot, app.ProxyPort = as.Slot, as.ProxyPort

	if w.Release != "" {
		if err := a.release(ctx, r, ps, name, app, out); err != nil {
			return err
		}
	}
	c, err := a.Node.StartCandidate(ctx, app, node.Next, out)
	if err != nil {
		return err
	}
	// The state first: after a crash between the two, the slot it names is
	// the live one, and the other is a leftover to remove.
	prev := *as
	as.Slot, as.Port, as.Previous, as.Image = c.Slot, c.Port, as.Image, ref
	as.PreviousID, as.ImageID = as.ImageID, imageID
	if err := save(); err != nil {
		*as = prev
		return a.Node.Discard(ctx, app, c, out, err)
	}
	if err := a.Node.Switch(ctx, app, c); err != nil {
		*as = prev
		if serr := save(); serr != nil {
			fmt.Fprintln(out, "warning:", serr)
		}
		return a.Node.Discard(ctx, app, c, out, err)
	}
	a.Node.Retire(ctx, app, c, out)
	if err := a.Node.Promote(ctx, key); err != nil {
		fmt.Fprintln(out, "warning:", err)
	}
	fmt.Fprintf(out, "%s is live on 127.0.0.1:%d\n", name, as.ProxyPort)

	if err := a.Node.RemoveWorker(ctx, key); err != nil {
		return err
	}
	if w.Worker != "" {
		if err := a.runWorker(ctx, r, ps, name, out); err != nil {
			return fmt.Errorf("worker: %w", err)
		}
	}
	return nil
}

// release saves a dump of each database the app uses, then runs its
// release command in the new image.
func (a *Applier) release(ctx context.Context, r Release, ps *ProjectState, name string, app node.AppSpec, out io.Writer) error {
	project := r.Spec.Name
	for _, db := range sortedKeys(r.Spec.Apps[name].Uses) {
		if _, ok := r.Spec.Databases[db]; !ok {
			continue
		}
		d, err := a.database(ps, project, db, func() error { return nil })
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "saving a dump of", db, "before the release command")
		if err := a.snapshot(ctx, d); err != nil {
			return fmt.Errorf("dump of %s: %w", db, err)
		}
	}
	fmt.Fprintln(out, "running", r.Spec.Apps[name].Release)
	run := a.RunOnce
	if run == nil {
		timeout := a.ReleaseTimeout
		if timeout == 0 {
			timeout = 30 * time.Minute
		}
		run = func(ctx context.Context, tag, cname, command string, opts deploy.AppOptions) (string, error) {
			return deploy.RunOnce(ctx, tag, cname, command, opts, timeout)
		}
	}
	output, err := run(ctx, node.Tag(app.Name, node.Next), app.Name+"-release", r.Spec.Apps[name].Release, deploy.AppOptions{
		App: app.Name, Network: node.ProjectNetwork(project), Env: app.Env, Binds: app.Binds, MemoryMB: app.MemoryMB, CPUs: app.CPUs,
	})
	fmt.Fprint(out, output)
	if err != nil {
		// Its last words go in the error, so the commit's status and the
		// mail say why without opening the log.
		if last := lastLine(output); last != "" {
			return fmt.Errorf("%w: %s; the live version keeps running", err, last)
		}
		return fmt.Errorf("%w; the live version keeps running", err)
	}
	return nil
}

func (a *Applier) runWorker(ctx context.Context, r Release, ps *ProjectState, name string, out io.Writer) error {
	app := a.appSpec(ctx, r, ps, name)
	return a.Node.RunWorker(ctx, node.WorkerSpec{App: app, Command: r.Spec.Apps[name].Worker}, out)
}

// appSpec is what the node needs to run the app: its variables (from
// uses, the file and its secrets), told to listen on its port, and its
// volumes.
func (a *Applier) appSpec(ctx context.Context, r Release, ps *ProjectState, name string) node.AppSpec {
	project, w := r.Spec.Name, r.Spec.Apps[name]
	key := AppKey(project, name)
	hint := int64(w.Port)
	if hint == 0 {
		hint = a.Node.ExposedPort(ctx, key, node.Next)
	}
	if hint == 0 {
		hint = 8080
	}
	var env []string
	for _, res := range sortedKeys(w.Uses) {
		prefix := w.Uses[res]
		if _, ok := r.Spec.Databases[res]; ok {
			key := DBKey(project, res)
			env = append(env, values(spec.Vars("database", prefix), []string{
				fmt.Sprintf("postgres://%s:%s@%s:5432/%s", key, ps.Databases[res].Password, node.PostgresContainer, key),
				node.PostgresContainer, "5432", key, key, ps.Databases[res].Password,
			})...)
		} else {
			host, pw := CacheKey(project, res), ps.Caches[res].Password
			url := fmt.Sprintf("redis://default:%s@%s:6379/0", pw, host)
			env = append(env, values(spec.Vars("cache", prefix), []string{url, url, host, "6379", pw})...)
		}
	}
	// What hakobu gives the app, which the file's variables may name as
	// ${NAME}.
	env = append(env, fmt.Sprintf("PORT=%d", hint))
	if r.Revision != "" {
		env = append(env, spec.RevisionVar+"="+r.Revision)
	}
	given := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		given[k] = v
	}
	for _, k := range sortedKeys(w.Env) {
		env = append(env, k+"="+spec.Expand(w.Env[k], given))
	}
	for _, k := range sortedKeys(r.Secrets[name]) {
		env = append(env, k+"="+r.Secrets[name][k])
	}
	var binds []string
	for _, v := range sortedKeys(w.Volumes) {
		binds = append(binds, node.Volume(key, v)+":"+w.Volumes[v])
	}
	return node.AppSpec{
		Name: key, Project: project, ContainerPort: int64(w.Port), PortHint: hint,
		HealthPath: w.Health, MemoryMB: int64(w.Memory >> 20), CPUs: w.CPUs,
		Env: env, Binds: binds, Recreate: len(binds) > 0,
	}
}

// snapshot writes the database's dump to SnapshotDir, replacing the last
// one only once the new one is whole.
func (a *Applier) snapshot(ctx context.Context, d node.DBSpec) error {
	dir := a.SnapshotDir
	if dir == "" {
		dir = "snapshots"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".dump-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = a.Node.DumpDatabase(ctx, d, f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, d.Name+".dump"))
}

func values(names, vals []string) []string {
	out := make([]string, len(names))
	for i := range names {
		out[i] = names[i] + "=" + vals[i]
	}
	return out
}

func (a *Applier) pull(ctx context.Context, r Release, ref, tag string) error {
	if a.Pull != nil {
		return a.Pull(ctx, ref, tag)
	}
	auth, fromCI := a.authFor(r, ref)
	if err := deploy.PullImage(ctx, ref, auth); err != nil {
		if fromCI {
			return fmt.Errorf("%w (with the token CI sealed in the release, which works only while its job runs: run the workflow again, or give the server credentials for %s)", err, r.Pull.Host)
		}
		return err
	}
	return deploy.TagImage(ctx, ref, tag)
}

// authFor is how to sign in to pull ref: the server's own credentials for
// its registry, else the token CI sealed in the release (fromCI).
func (a *Applier) authFor(r Release, ref string) (auth deploy.RegistryAuth, fromCI bool) {
	img, err := registry.ParseRef(ref)
	if err != nil {
		return auth, false
	}
	if auth, ok := a.Auth[img.Host]; ok {
		return auth, false
	}
	if r.Pull != nil && r.Pull.Host == img.Host {
		return deploy.RegistryAuth{Username: r.Pull.Username, Password: r.Pull.Password}, true
	}
	return auth, false
}

// localImage is the ID of an image of the app's this server has already
// for ref, live or previous, "" if none: Docker can't always find an
// image by the digest it was pulled by.
func (a *Applier) localImage(ctx context.Context, ref string, as *AppState) string {
	if a.Pull != nil {
		return "" // tests pull into a fake
	}
	for _, c := range []struct{ ref, id string }{{as.Image, as.ImageID}, {as.Previous, as.PreviousID}} {
		if c.ref == ref && c.id != "" {
			if here, _ := deploy.ImageExists(ctx, c.id); here {
				return c.id
			}
		}
	}
	return ""
}

func (a *Applier) imageID(ctx context.Context, tag string) string {
	if a.Pull != nil {
		return ""
	}
	return deploy.ImageID(ctx, tag)
}

// freePort is the lowest proxy port no app on the server has.
func (s *State) freePort() int64 {
	used := map[int64]bool{}
	for _, p := range s.Projects {
		for _, a := range p.Apps {
			used[a.ProxyPort] = true
		}
	}
	port := int64(FirstProxyPort)
	for used[port] {
		port++
	}
	return port
}

func randomHex() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on Linux
	}
	return hex.EncodeToString(b)
}

// lastLine is the last line of output that says something, without the
// time Docker puts before each.
func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if at, rest, ok := strings.Cut(last, " "); ok {
		if _, err := time.Parse(time.RFC3339Nano, at); err == nil {
			last = strings.TrimSpace(rest)
		}
	}
	return last
}
