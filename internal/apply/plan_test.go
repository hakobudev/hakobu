package apply

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/spec"
)

func parse(t *testing.T, s string) *spec.Project {
	t.Helper()
	p, err := spec.Parse([]byte("project = \"kasl\"\nserver = \"kasl-1\"\n" + s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const crm = `
[apps.crm]
domain = "crm.kaslauto.com"
worker = "bun run worker"
uses = ["main", "cache"]
volumes = { uploads = "/app/data" }
env = { NODE_ENV = "production" }
[databases.main]
[caches.cache]
`

// applied is the state after want went live on image.
func applied(want *spec.Project, image string) *ProjectState {
	s := (&State{Projects: map[string]*ProjectState{}}).Project(want.Name)
	s.Spec = want
	for n, a := range want.Apps {
		s.Apps[n] = &AppState{Slot: "blue", ProxyPort: 20000, Image: image}
		for v := range a.Volumes {
			s.Volumes[n+"/"+v] = true
		}
	}
	return s
}

func lines(p Plan) string {
	var b []string
	for _, c := range p.Changes {
		b = append(b, string(c.Action)+" "+c.Kind+" "+c.Name)
	}
	return strings.Join(b, "\n")
}

func TestPlanFirstDeploy(t *testing.T) {
	p, err := MakePlan(parse(t, crm), map[string]string{"crm": "sha256:aaaa"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "+ database main\n+ cache cache\n+ volume crm/uploads\n+ app crm\n+ worker crm"
	if got := lines(p); got != want {
		t.Errorf("plan:\n%s\nwant:\n%s", got, want)
	}
	if len(p.Redeploy) != 1 || p.Redeploy[0] != "crm" {
		t.Errorf("redeploy %v", p.Redeploy)
	}
}

func TestPlanNoChange(t *testing.T) {
	want := parse(t, crm)
	p, err := MakePlan(want, map[string]string{"crm": "sha256:aaaa"}, applied(want, "sha256:aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() || len(p.Redeploy) > 0 {
		t.Errorf("plan:\n%s", p)
	}
	if !strings.Contains(p.String(), "nothing to change") {
		t.Errorf("String = %q", p.String())
	}
}

func TestPlanNewImage(t *testing.T) {
	want := parse(t, crm)
	p, _ := MakePlan(want, map[string]string{"crm": "sha256:bbbbbbbbbbbbbbbbbbbb"}, applied(want, "sha256:aaaaaaaaaaaaaaaa"))
	if len(p.Changes) != 1 || p.Changes[0].Detail != "image aaaaaaaaaaaa → bbbbbbbbbbbb" {
		t.Errorf("plan:\n%s", p)
	}
	if len(p.Redeploy) != 1 {
		t.Errorf("redeploy %v", p.Redeploy)
	}
}

func TestPlanSettings(t *testing.T) {
	old := parse(t, crm)
	want := parse(t, strings.NewReplacer(
		`worker = "bun run worker"`, `worker = "bun run worker --concurrency 4"`+"\nmemory = \"1gb\"",
		`env = { NODE_ENV = "production" }`, `env = { NODE_ENV = "production", SECRET_THING = "x" }`,
	).Replace(crm))
	p, _ := MakePlan(want, map[string]string{"crm": "sha256:aaaa"}, applied(old, "sha256:aaaa"))
	if len(p.Changes) != 1 {
		t.Fatalf("plan:\n%s", p)
	}
	d := p.Changes[0].Detail
	for _, s := range []string{"worker bun run worker → bun run worker --concurrency 4", "memory no limit → 1gb", "env SECRET_THING"} {
		if !strings.Contains(d, s) {
			t.Errorf("%q missing from %q", s, d)
		}
	}
	if strings.Contains(d, "\"x\"") || strings.Contains(d, "= x") {
		t.Errorf("a variable's value shows: %q", d)
	}
}

func TestPlanKeepsData(t *testing.T) {
	old := parse(t, crm)
	want := parse(t, "[apps.crm]\n")
	p, err := MakePlan(want, map[string]string{"crm": "sha256:aaaa"}, applied(old, "sha256:aaaa"))
	if err != nil {
		t.Fatal(err)
	}
	orphans := p.Orphans()
	if len(orphans) != 3 {
		t.Fatalf("orphans %v, plan:\n%s", orphans, p)
	}
	for _, c := range orphans {
		if !strings.Contains(c.Detail, "hakobu destroy kasl/") {
			t.Errorf("%s doesn't say how to drop it", c)
		}
	}
	if !strings.Contains(lines(p), "- worker crm") {
		t.Errorf("the worker isn't removed:\n%s", p)
	}
}

func TestPlanRemovesApp(t *testing.T) {
	old := parse(t, crm+"[apps.admin]\n")
	want := parse(t, crm)
	images := map[string]string{"crm": "sha256:aaaa"}
	s := applied(old, "sha256:aaaa")
	p, _ := MakePlan(want, images, s)
	if got := lines(p); got != "- app admin" {
		t.Errorf("plan:\n%s", p)
	}
}

func TestPlanRefuses(t *testing.T) {
	old := parse(t, crm)
	renamed := parse(t, strings.NewReplacer(`"main"`, `"primary"`, "[databases.main]", "[databases.primary]").Replace(crm))
	if _, err := MakePlan(renamed, map[string]string{"crm": "sha256:a"}, applied(old, "sha256:a")); err == nil || !strings.Contains(err.Error(), "renaming a database") {
		t.Errorf("rename: %v", err)
	}
	moved := parse(t, crm)
	moved.Server = "kasl-2"
	if _, err := MakePlan(moved, map[string]string{"crm": "sha256:a"}, applied(old, "sha256:a")); err == nil || !strings.Contains(err.Error(), "moves from server") {
		t.Errorf("move: %v", err)
	}
	if _, err := MakePlan(parse(t, crm), nil, nil); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Errorf("no image: %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadState(file)
	if err != nil || len(s.Projects) != 0 {
		t.Fatalf("empty state: %v %v", s, err)
	}
	ps := s.Project("kasl")
	ps.Spec = parse(t, crm)
	ps.Apps["crm"] = &AppState{Slot: "green", ProxyPort: 20001, Image: "sha256:b", Previous: "sha256:a"}
	ps.Databases["main"] = &DatabaseState{Password: "pw"}
	if err := s.Save(file); err != nil {
		t.Fatal(err)
	}
	r, err := LoadState(file)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Projects["kasl"]
	if got.Apps["crm"].Slot != "green" || got.Databases["main"].Password != "pw" || got.Spec.Apps["crm"].Domain != "crm.kaslauto.com" {
		t.Errorf("read back %+v", got)
	}
	if got.Spec.Apps["crm"].Uses["main"] != "" || len(got.Spec.Apps["crm"].Uses) != 2 {
		t.Errorf("uses read back as %v", got.Spec.Apps["crm"].Uses)
	}
}
