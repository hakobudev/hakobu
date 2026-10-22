package apply

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/hakobudev/hakobu/internal/spec"
)

// Action is what a deploy does to one thing.
type Action string

const (
	Create Action = "+"
	Update Action = "~"
	Remove Action = "-" // only what holds no data: an app, its worker
	Orphan Action = "!" // data the file no longer has: kept until destroyed
)

// Change is one line of a plan.
type Change struct {
	Action Action
	Kind   string // "app", "worker", "database", "cache", "volume"
	Name   string // "crm", or "crm/uploads" for a volume
	Detail string
}

func (c Change) String() string {
	s := fmt.Sprintf("%s %-8s %s", c.Action, c.Kind, c.Name)
	if c.Detail != "" {
		s += "  " + c.Detail
	}
	return s
}

// Plan is what applying a spec, with images, would change on the server.
type Plan struct {
	Project string
	Changes []Change
	// Redeploy are the apps that get a new container: new, changed or on
	// a new image.
	Redeploy []string
}

// Empty tells whether applying would change nothing.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

func (p Plan) String() string {
	if p.Empty() {
		return p.Project + ": nothing to change\n"
	}
	var b strings.Builder
	b.WriteString(p.Project + "\n")
	for _, c := range p.Changes {
		b.WriteString("  " + c.String() + "\n")
	}
	return b.String()
}

// Orphans are the plan's lines about data the file no longer has.
func (p Plan) Orphans() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Action == Orphan {
			out = append(out, c)
		}
	}
	return out
}

// MakePlan compares want, with the image digest each app should run, to
// what was last applied. It refuses what it can't do safely; everything
// else is in the plan, orphans included.
func MakePlan(want *spec.Project, images map[string]string, have *ProjectState) (Plan, error) {
	p := Plan{Project: want.Name}
	var last spec.Project
	if have != nil && have.Spec != nil {
		last = *have.Spec
	}
	if last.Name != "" && last.Server != want.Server {
		return p, fmt.Errorf("the project moves from server %s to %s: deploy it there anew, then destroy it here", last.Server, want.Server)
	}
	if err := refuseRenames(want, &last); err != nil {
		return p, err
	}
	add := func(a Action, kind, name, detail string) {
		p.Changes = append(p.Changes, Change{a, kind, name, detail})
	}

	for _, n := range sortedKeys(want.Databases) {
		if _, ok := last.Databases[n]; !ok {
			add(Create, "database", n, "")
		}
	}
	for _, n := range sortedKeys(last.Databases) {
		if _, ok := want.Databases[n]; !ok {
			add(Orphan, "database", n, "not in the file: kept; hakobu destroy "+want.Name+"/"+n+" to drop it")
		}
	}
	for _, n := range sortedKeys(want.Caches) {
		w := want.Caches[n]
		l, ok := last.Caches[n]
		switch {
		case !ok:
			add(Create, "cache", n, w.Memory.String())
		case w.Memory != l.Memory:
			detail := "memory " + l.Memory.String() + " → " + w.Memory.String()
			if w.Memory < l.Memory {
				detail += " (smaller: keys past it are evicted)"
			}
			add(Update, "cache", n, detail)
		}
	}
	for _, n := range sortedKeys(last.Caches) {
		if _, ok := want.Caches[n]; !ok {
			add(Orphan, "cache", n, "not in the file: kept; hakobu destroy "+want.Name+"/"+n+" to drop it")
		}
	}

	volumes := map[string]bool{}
	for _, a := range sortedKeys(want.Apps) {
		for _, v := range sortedKeys(want.Apps[a].Volumes) {
			volumes[a+"/"+v] = true
		}
	}
	made := map[string]bool{}
	if have != nil {
		made = have.Volumes
	}
	for _, v := range sortedKeys(volumes) {
		if !made[v] {
			add(Create, "volume", v, "")
		}
	}
	for _, v := range sortedKeys(made) {
		if !volumes[v] {
			add(Orphan, "volume", v, "not in the file: kept; hakobu destroy "+want.Name+"/"+v+" to drop it")
		}
	}

	for _, n := range sortedKeys(want.Apps) {
		w := want.Apps[n]
		image := images[n]
		l, existed := last.Apps[n]
		var live string
		if have != nil && have.Apps[n] != nil {
			live = have.Apps[n].Image
		}
		if !existed {
			if image == "" {
				return p, fmt.Errorf("apps.%s: no image to start it from", n)
			}
			add(Create, "app", n, appSummary(w, image))
			if w.Worker != "" {
				add(Create, "worker", n, w.Worker)
			}
			p.Redeploy = append(p.Redeploy, n)
			continue
		}
		diffs := appDiff(l, w)
		if image != "" && image != live {
			diffs = append([]string{"image " + short(live) + " → " + short(image)}, diffs...)
		}
		if len(diffs) > 0 {
			add(Update, "app", n, strings.Join(diffs, ", "))
			p.Redeploy = append(p.Redeploy, n)
		}
		switch {
		case l.Worker == "" && w.Worker != "":
			add(Create, "worker", n, w.Worker)
		case l.Worker != "" && w.Worker == "":
			add(Remove, "worker", n, "")
		}
	}
	for _, n := range sortedKeys(last.Apps) {
		if _, ok := want.Apps[n]; !ok {
			add(Remove, "app", n, "its containers and images go; its volumes stay")
			if last.Apps[n].Worker != "" {
				add(Remove, "worker", n, "")
			}
		}
	}
	return p, nil
}

// refuseRenames stops a plan that would look like dropping one database
// and making another: a rename isn't supported, and doing it as written
// would leave the data behind.
func refuseRenames(want, last *spec.Project) error {
	var gone, added []string
	for _, n := range sortedKeys(last.Databases) {
		if _, ok := want.Databases[n]; !ok {
			gone = append(gone, n)
		}
	}
	for _, n := range sortedKeys(want.Databases) {
		if _, ok := last.Databases[n]; !ok {
			added = append(added, n)
		}
	}
	if len(gone) > 0 && len(added) > 0 {
		return fmt.Errorf("databases %s leave the file as %s come in: renaming a database isn't supported, and this would leave its data behind; deploy the new one first, move the data, then take the old one out", strings.Join(gone, ", "), strings.Join(added, ", "))
	}
	return nil
}

// appDiff names what changed in an app's settings, in the file's terms.
func appDiff(l, w spec.App) []string {
	var d []string
	field := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			d = append(d, fmt.Sprintf("%s %v → %v", name, show(a), show(b)))
		}
	}
	field("domain", l.Domain, w.Domain)
	field("health", l.Health, w.Health)
	field("port", l.Port, w.Port)
	field("release", l.Release, w.Release)
	if l.Worker != "" && w.Worker != "" {
		field("worker", l.Worker, w.Worker)
	}
	field("memory", l.Memory, w.Memory)
	field("cpus", l.CPUs, w.CPUs)
	field("path", l.Path, w.Path)
	if !maps.Equal(l.Volumes, w.Volumes) {
		d = append(d, "volumes")
	}
	if !maps.Equal(l.Uses, w.Uses) {
		d = append(d, "uses "+usesList(l.Uses)+" → "+usesList(w.Uses))
	}
	// Variables' values may be secret-adjacent: name the keys only.
	if keys := changedKeys(l.Env, w.Env); len(keys) > 0 {
		d = append(d, "env "+strings.Join(keys, ", "))
	}
	return d
}

func appSummary(a spec.App, image string) string {
	parts := []string{"image " + short(image)}
	if a.Domain != "" {
		parts = append(parts, a.Domain)
	}
	if len(a.Uses) > 0 {
		parts = append(parts, "uses "+usesList(a.Uses))
	}
	return strings.Join(parts, ", ")
}

func usesList(u spec.Uses) string {
	if len(u) == 0 {
		return "nothing"
	}
	return strings.Join(sortedKeys(u), " ")
}

func changedKeys(a, b map[string]string) []string {
	var keys []string
	for _, k := range sortedKeys(a) {
		if bv, ok := b[k]; !ok || bv != a[k] {
			keys = append(keys, k)
		}
	}
	for _, k := range sortedKeys(b) {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func show(v any) any {
	switch v := v.(type) {
	case string:
		if v == "" {
			return "none"
		}
		return v
	case spec.Size:
		if v == 0 {
			return "no limit"
		}
		return v.String()
	case int:
		if v == 0 {
			return "auto"
		}
	case float64:
		if v == 0 {
			return "no limit"
		}
	}
	return v
}

// short is a digest as people compare them: its first 12 hex digits.
func short(digest string) string {
	if digest == "" {
		return "none"
	}
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
