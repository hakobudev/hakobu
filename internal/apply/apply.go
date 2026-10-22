package apply

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/spec"
)

// Release is one commit of a project as the server deploys it.
type Release struct {
	Spec *spec.Project
	// Images is the image each app runs, pinned by digest.
	Images map[string]string
	// Secrets are each app's secret variables, decrypted.
	Secrets map[string]map[string]string
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
	SaveDump(ctx context.Context, d node.DBSpec) (node.Dump, error)
	KeepSnapshot(app string, dump node.Dump) error
	EnsureProjectNetworks(ctx context.Context, project string, tunnels []node.TunnelSpec) error
}

// Applier deploys releases on this server, one at a time.
type Applier struct {
	Node      Node
	StateFile string
	// Pull fetches an app's image and tags it as tag; nil pulls with
	// Docker as Auth.
	Pull func(ctx context.Context, ref, tag string) error
	Auth deploy.RegistryAuth
	// RunOnce runs a release command; nil runs it with Docker.
	RunOnce func(ctx context.Context, tag, name, command string, opts deploy.AppOptions) (string, error)
	// ReleaseTimeout bounds a release command (default 30 minutes).
	ReleaseTimeout time.Duration

	mu sync.Mutex
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

// Apply brings the server in line with r and returns what it changed. On
// an error the apps it didn't reach keep running as they were, and the
// state keeps what was made, so applying again picks up where it stopped.
func (a *Applier) Apply(ctx context.Context, r Release, out io.Writer) (Plan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := LoadState(a.StateFile)
	if err != nil {
		return Plan{}, err
	}
	ps := st.Project(r.Spec.Name)
	plan, err := MakePlan(r.Spec, r.Images, ps)
	if err != nil {
		return plan, err
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
	if err := a.Node.EnsureProjectNetworks(ctx, project, nil); err != nil {
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
	fmt.Fprintln(out, "pulling", ref)
	if err := a.pull(ctx, ref, node.Tag(key, node.Next)); err != nil {
		return err
	}
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
	as.Slot, as.Previous, as.Image = c.Slot, as.Image, ref
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

// release saves a dump of each database the app uses, as the app's
// snapshot, then runs its release command in the new image.
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
		dump, err := a.Node.SaveDump(ctx, d)
		if err != nil {
			return fmt.Errorf("dump of %s: %w", db, err)
		}
		if err := a.Node.KeepSnapshot(app.Name+"-"+db, dump); err != nil {
			return err
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
		return fmt.Errorf("release command: %w; the live version keeps running", err)
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
	for _, k := range sortedKeys(w.Env) {
		env = append(env, k+"="+w.Env[k])
	}
	for _, k := range sortedKeys(r.Secrets[name]) {
		env = append(env, k+"="+r.Secrets[name][k])
	}
	env = append(env, fmt.Sprintf("PORT=%d", hint))
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

func values(names, vals []string) []string {
	out := make([]string, len(names))
	for i := range names {
		out[i] = names[i] + "=" + vals[i]
	}
	return out
}

func (a *Applier) pull(ctx context.Context, ref, tag string) error {
	if a.Pull != nil {
		return a.Pull(ctx, ref, tag)
	}
	if err := deploy.PullImage(ctx, ref, a.Auth); err != nil {
		return err
	}
	return deploy.TagImage(ctx, ref, tag)
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
