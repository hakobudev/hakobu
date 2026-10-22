package apply

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/spec"
)

// Resource is a database, cache or volume of a project, as the state
// holds it.
type Resource struct {
	Project string
	Kind    string // "database", "cache", "volume"
	Name    string // "main", or "crm/uploads" for a volume
	// DB is a database's spec on the server.
	DB node.DBSpec
	// App and Volume name a volume on the server.
	App, Volume string
	// InFile tells whether hakobu.toml still has it.
	InFile bool
}

// Resources are every database, cache and volume hakobu made, orphans
// included, project by project.
func (a *Applier) Resources() ([]Resource, error) {
	st, err := a.read()
	if err != nil {
		return nil, err
	}
	var out []Resource
	for _, project := range sortedKeys(st.Projects) {
		out = append(out, resources(project, st.Project(project))...)
	}
	return out, nil
}

func resources(project string, ps *ProjectState) []Resource {
	spec := ps.Spec
	if spec == nil {
		spec = &specProject{} // nothing applied yet: nothing is in a file
	}
	var out []Resource
	for _, n := range sortedKeys(ps.Databases) {
		key := DBKey(project, n)
		_, in := spec.Databases[n]
		out = append(out, Resource{Project: project, Kind: "database", Name: n, InFile: in,
			DB: node.DBSpec{Name: key, User: key, Password: ps.Databases[n].Password}})
	}
	for _, n := range sortedKeys(ps.Caches) {
		_, in := spec.Caches[n]
		out = append(out, Resource{Project: project, Kind: "cache", Name: n, InFile: in})
	}
	for _, v := range sortedKeys(ps.Volumes) {
		app, vol, _ := strings.Cut(v, "/")
		_, in := spec.Apps[app].Volumes[vol]
		out = append(out, Resource{Project: project, Kind: "volume", Name: v, App: AppKey(project, app), Volume: vol, InFile: in})
	}
	return out
}

type specProject = spec.Project

// Destroyer is what destroying data needs of the machine.
type Destroyer interface {
	DropDatabase(ctx context.Context, d node.DBSpec) error
	DropCache(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, app, name string) error
}

// Destroy deletes an orphan: a database, cache or volume hakobu.toml no
// longer has. One the file still has is refused, as deploying would make
// it again. lastBackup runs first and must succeed, so nothing is
// destroyed without a backup of it.
func (a *Applier) Destroy(ctx context.Context, d Destroyer, project, name string, lastBackup func(Resource) error, out io.Writer) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := LoadState(a.StateFile)
	if err != nil {
		return err
	}
	if _, ok := st.Projects[project]; !ok {
		return fmt.Errorf("no project %s on this server", project)
	}
	ps := st.Project(project)
	var found *Resource
	for _, r := range resources(project, ps) {
		if r.Name == name {
			found = &r
			break
		}
	}
	if found == nil {
		return fmt.Errorf("%s/%s: no such database, cache or volume", project, name)
	}
	if found.InFile {
		return fmt.Errorf("%s/%s is still in hakobu.toml: take it out and deploy first, or the next deploy makes it again", project, name)
	}
	if lastBackup != nil {
		fmt.Fprintln(out, "backing up", project+"/"+name, "one last time")
		if err := lastBackup(*found); err != nil {
			return fmt.Errorf("the last backup failed, so nothing was destroyed: %w", err)
		}
	}
	fmt.Fprintln(out, "destroying", found.Kind, project+"/"+name)
	switch found.Kind {
	case "database":
		err = d.DropDatabase(ctx, found.DB)
		delete(ps.Databases, name)
	case "cache":
		err = d.DropCache(ctx, CacheKey(project, name))
		delete(ps.Caches, name)
	case "volume":
		err = d.RemoveVolume(ctx, found.App, found.Volume)
		delete(ps.Volumes, name)
	}
	if err != nil {
		return err
	}
	return st.Save(a.StateFile)
}

// Proxier starts an app's proxy again (node.Local.EnsureProxy).
type Proxier interface {
	EnsureProxy(ctx context.Context, app node.AppSpec)
}

// Reconciler tidies up after a deploy hakobu didn't live to finish
// (node.Local.Reconcile).
type Reconciler interface {
	Reconcile(ctx context.Context, apps []node.AppSpec)
}

// Reconcile has r tidy every live app as hakobu starts: the slot the state
// doesn't name goes, and a live container a stopped deploy left stopped
// starts again.
func (a *Applier) Reconcile(ctx context.Context, r Reconciler) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := LoadState(a.StateFile)
	if err != nil {
		return err
	}
	var apps []node.AppSpec
	for _, project := range sortedKeys(st.Projects) {
		ps := st.Projects[project]
		for _, n := range sortedKeys(ps.Apps) {
			if as := ps.Apps[n]; as.Slot != "" {
				apps = append(apps, node.AppSpec{Name: AppKey(project, n), Project: project, ActiveSlot: as.Slot})
			}
		}
	}
	r.Reconcile(ctx, apps)
	return nil
}

// EnsureProxies starts every live app's proxy, as hakobu starts: they run
// in its process, and went with the last one.
func (a *Applier) EnsureProxies(ctx context.Context, p Proxier) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := LoadState(a.StateFile)
	if err != nil {
		return err
	}
	for _, project := range sortedKeys(st.Projects) {
		ps := st.Projects[project]
		for _, n := range sortedKeys(ps.Apps) {
			as := ps.Apps[n]
			if as.Slot == "" {
				continue
			}
			p.EnsureProxy(ctx, node.AppSpec{Name: AppKey(project, n), Project: project, ActiveSlot: as.Slot, ProxyPort: as.ProxyPort, PortHint: as.Port})
		}
	}
	return nil
}

// Live is the app's live container on the server and its spec, as a
// restore of its volume stops and starts it.
func (a *Applier) Live(project, app string) (node.AppSpec, error) {
	st, err := a.read()
	if err != nil {
		return node.AppSpec{}, err
	}
	ps, ok := st.Projects[project]
	if !ok || ps.Apps[app] == nil || ps.Apps[app].Slot == "" {
		return node.AppSpec{}, fmt.Errorf("%s/%s isn't running on this server", project, app)
	}
	as := ps.Apps[app]
	return node.AppSpec{Name: AppKey(project, app), Project: project, ActiveSlot: as.Slot, ProxyPort: as.ProxyPort, PortHint: as.Port}, nil
}

// Apps are the apps on the server, project by project.
func (a *Applier) Apps() (map[string]map[string]AppState, error) {
	st, err := a.read()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]AppState{}
	for p, ps := range st.Projects {
		out[p] = map[string]AppState{}
		for n, as := range ps.Apps {
			out[p][n] = *as
		}
	}
	return out, nil
}

// Specs are each app's health path, from the specs last applied.
func (a *Applier) Specs() (map[string]map[string]string, error) {
	st, err := a.read()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for p, ps := range st.Projects {
		if ps.Spec == nil {
			continue
		}
		out[p] = map[string]string{}
		for n, app := range ps.Spec.Apps {
			out[p][n] = app.Health
		}
	}
	return out, nil
}

// Domains are each app domain's proxy port, for the ingress, which asks on
// every request: they're read again only once the state file changed.
func (a *Applier) Domains() map[string]int64 {
	fi, err := os.Stat(a.StateFile)
	if err != nil {
		return nil
	}
	a.routesMu.Lock()
	defer a.routesMu.Unlock()
	stamp := fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
	if a.routes != nil && stamp == a.routesStamp {
		return a.routes
	}
	st, err := a.read()
	if err != nil {
		return a.routes // the last ones known
	}
	out := map[string]int64{}
	for _, ps := range st.Projects {
		if ps.Spec == nil {
			continue
		}
		for n, app := range ps.Spec.Apps {
			if as := ps.Apps[n]; app.Domain != "" && as != nil && as.Slot != "" {
				out[app.Domain] = as.ProxyPort
			}
		}
	}
	a.routes, a.routesStamp = out, stamp
	return out
}

// read is the state as last saved, without waiting for a deploy: Save
// replaces the file whole, so a read never sees half of one.
func (a *Applier) read() (*State, error) { return LoadState(a.StateFile) }
