package apply

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/spec"
)

type harness struct {
	f        *nodetest.Fake
	a        *Applier
	released []string // release commands run, with the image they ran in
	releases []deploy.AppOptions
	failRun  error
}

func newHarness(t *testing.T) *harness {
	h := &harness{f: nodetest.New()}
	h.a = &Applier{
		Node:        h.f,
		StateFile:   filepath.Join(t.TempDir(), "state.json"),
		SnapshotDir: filepath.Join(t.TempDir(), "snapshots"),
		// A pull makes the image the app's Next build, as Docker would.
		Pull: func(_ context.Context, ref, tag string) error {
			app := strings.TrimPrefix(strings.TrimSuffix(tag, ":"+string(node.Next)), "hakobu/")
			if h.f.Images[app] == nil {
				h.f.Images[app] = map[node.Image]string{}
			}
			h.f.Images[app][node.Next] = ref
			return nil
		},
		RunOnce: func(_ context.Context, tag, name, command string, opts deploy.AppOptions) (string, error) {
			h.released = append(h.released, command+" in "+tag)
			h.releases = append(h.releases, opts)
			return "migrated\n", h.failRun
		},
	}
	return h
}

func (h *harness) apply(t *testing.T, toml string, images map[string]string) (Plan, error) {
	t.Helper()
	return h.a.Apply(context.Background(), Release{Spec: parse(t, toml), Images: images}, io.Discard)
}

// deploy is apply for a step that must work.
func (h *harness) deploy(t *testing.T, toml string, images map[string]string) {
	t.Helper()
	if _, err := h.apply(t, toml, images); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) state(t *testing.T) *ProjectState {
	t.Helper()
	s, err := LoadState(h.a.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	return s.Projects["kasl"]
}

func TestApplyFirstDeploy(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, `worker = "bun run worker"`, `worker = "bun run worker"`+"\nrelease = \"bun run migrate\"", 1)
	if _, err := h.apply(t, toml, map[string]string{"crm": "reg/crm@sha256:aaa"}); err != nil {
		t.Fatal(err)
	}
	f := h.f
	if f.Proxies["kasl-crm"] != "kasl-crm-green" || f.Containers["kasl-crm-green"] != "running" {
		t.Errorf("live: proxies %v, containers %v", f.Proxies, f.Containers)
	}
	if f.Runs["kasl-crm-worker"] != "reg/crm@sha256:aaa" {
		t.Errorf("worker runs %q", f.Runs["kasl-crm-worker"])
	}
	if _, ok := f.DBs["kasl_main"]; !ok {
		t.Errorf("no database kasl_main: %v", f.DBs)
	}
	if c := f.Caches["kasl-cache"]; c.MemoryMB != 256 || c.Password == "" {
		t.Errorf("cache %+v", c)
	}
	if len(h.released) != 1 || h.released[0] != "bun run migrate in hakobu/kasl-crm:next" {
		t.Errorf("release commands %v", h.released)
	}
	s := h.state(t)
	if a := s.Apps["crm"]; a.Slot != "green" || a.Image != "reg/crm@sha256:aaa" || a.ProxyPort != FirstProxyPort {
		t.Errorf("state %+v", a)
	}
	env := strings.Join(h.releases[0].Env, "\n")
	pw := s.Databases["main"].Password
	for _, want := range []string{
		"DATABASE_URL=postgres://kasl_main:" + pw + "@hakobu-postgres:5432/kasl_main",
		"POSTGRES_PASSWORD=" + pw,
		"REDIS_URL=redis://default:" + s.Caches["cache"].Password + "@kasl-cache:6379/0",
		"NODE_ENV=production",
		"PORT=8080",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("%q missing from the env:\n%s", want, env)
		}
	}
	if !slices.Contains(h.releases[0].Binds, "hakobu_vol_kasl-crm_uploads:/app/data") && !strings.Contains(strings.Join(h.releases[0].Binds, " "), "kasl-crm") {
		t.Errorf("binds %v", h.releases[0].Binds)
	}
	if _, err := os.Stat(filepath.Join(h.a.SnapshotDir, "kasl_main.dump")); err != nil {
		t.Errorf("no dump before the release command: %v", err)
	}
}

func TestAppGetsItsRevision(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, `worker = "bun run worker"`, `worker = "bun run worker"`+"\nrelease = \"bun run migrate\"", 1)
	r := Release{Spec: parse(t, toml), Images: map[string]string{"crm": "reg/crm@sha256:aaa"}, Revision: "0123abc"}
	if _, err := h.a.Apply(context.Background(), r, io.Discard); err != nil {
		t.Fatal(err)
	}
	if env := h.releases[0].Env; !slices.Contains(env, "HAKOBU_REVISION=0123abc") {
		t.Errorf("no HAKOBU_REVISION in the env: %v", env)
	}
}

func TestEnvNamesWhatHakobuGives(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, `worker = "bun run worker"`, `worker = "bun run worker"`+"\nrelease = \"bun run migrate\"", 1)
	toml = strings.Replace(toml, `env = { NODE_ENV = "production" }`, `env = { DB_URI = "${DATABASE_URL}?schema=x", SELF = "http://crm:${PORT}" }`, 1)
	if _, err := h.apply(t, toml, map[string]string{"crm": "reg/crm@sha256:aaa"}); err != nil {
		t.Fatal(err)
	}
	pw := h.state(t).Databases["main"].Password
	env := h.releases[0].Env
	for _, want := range []string{"DB_URI=postgres://kasl_main:" + pw + "@hakobu-postgres:5432/kasl_main?schema=x", "SELF=http://crm:8080"} {
		if !slices.Contains(env, want) {
			t.Errorf("%q missing from the env: %v", want, env)
		}
	}
}

func TestApplyAgainChangesNothing(t *testing.T) {
	h := newHarness(t)
	images := map[string]string{"crm": "reg/crm@sha256:aaa"}
	if _, err := h.apply(t, crm, images); err != nil {
		t.Fatal(err)
	}
	before := len(h.f.Calls())
	p, err := h.apply(t, crm, images)
	if err != nil || !p.Empty() {
		t.Fatalf("plan %v, %v", p, err)
	}
	if after := h.f.Calls(); len(after) != before {
		t.Errorf("an empty plan still called %v", after[before:])
	}
}

func TestApplyNewImageSwapsSlots(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "reg/crm@sha256:aaa"})
	if _, err := h.apply(t, crm, map[string]string{"crm": "reg/crm@sha256:bbb"}); err != nil {
		t.Fatal(err)
	}
	if h.f.Proxies["kasl-crm"] != "kasl-crm-blue" || h.f.Containers["kasl-crm-green"] != "" {
		t.Errorf("proxies %v, containers %v", h.f.Proxies, h.f.Containers)
	}
	a := h.state(t).Apps["crm"]
	if a.Image != "reg/crm@sha256:bbb" || a.Previous != "reg/crm@sha256:aaa" || a.Slot != "blue" {
		t.Errorf("state %+v", a)
	}
}

func TestApplyFailedReleaseKeepsLive(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"migrate\"", 1)
	h.deploy(t, toml, map[string]string{"crm": "reg/crm@sha256:aaa"})
	h.failRun = deploy.ErrReleaseFailed
	_, err := h.apply(t, toml, map[string]string{"crm": "reg/crm@sha256:bbb"})
	if !errors.Is(err, deploy.ErrReleaseFailed) || !strings.Contains(err.Error(), "live version keeps running") {
		t.Fatalf("err = %v", err)
	}
	if h.f.Proxies["kasl-crm"] != "kasl-crm-green" {
		t.Errorf("proxy moved: %v", h.f.Proxies)
	}
	s := h.state(t)
	if s.Apps["crm"].Image != "reg/crm@sha256:aaa" {
		t.Errorf("state took the failed image: %+v", s.Apps["crm"])
	}
	// The spec stays the old one, so the next try plans the app again.
	h.failRun = nil
	p, err := h.apply(t, toml, map[string]string{"crm": "reg/crm@sha256:bbb"})
	if err != nil || len(p.Redeploy) != 1 {
		t.Errorf("retry: %v, %v", p, err)
	}
}

func TestApplyUnhealthyKeepsLive(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "reg/crm@sha256:aaa"})
	h.f.Fail("StartCandidate", errors.New("new version didn't answer"))
	if _, err := h.apply(t, crm, map[string]string{"crm": "reg/crm@sha256:bbb"}); err == nil {
		t.Fatal("no error")
	}
	if h.f.Proxies["kasl-crm"] != "kasl-crm-green" || h.state(t).Apps["crm"].Image != "reg/crm@sha256:aaa" {
		t.Errorf("proxies %v, state %+v", h.f.Proxies, h.state(t).Apps["crm"])
	}
}

func TestApplyFailedSwitchRestoresState(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "reg/crm@sha256:aaa"})
	h.f.Fail("Switch", errors.New("proxy gone"))
	if _, err := h.apply(t, crm, map[string]string{"crm": "reg/crm@sha256:bbb"}); err == nil {
		t.Fatal("no error")
	}
	if a := h.state(t).Apps["crm"]; a.Slot != "green" || a.Image != "reg/crm@sha256:aaa" {
		t.Errorf("state %+v", a)
	}
	if !h.f.Called("Discard") {
		t.Error("the candidate wasn't discarded")
	}
}

func TestApplyKeepsDataAndRemovesApps(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm+"[apps.admin]\n", map[string]string{"crm": "reg/crm@sha256:aaa", "admin": "reg/admin@sha256:111"})
	p, err := h.apply(t, "[apps.crm]\n", map[string]string{"crm": "reg/crm@sha256:aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Orphans()) != 3 {
		t.Errorf("orphans %v", p.Orphans())
	}
	if _, ok := h.f.DBs["kasl_main"]; !ok {
		t.Error("the database went")
	}
	if !h.f.Called("RemoveApp") || h.state(t).Apps["admin"] != nil {
		t.Errorf("admin wasn't removed: %+v", h.state(t).Apps)
	}
	if h.f.Containers["kasl-crm-worker"] != "" {
		t.Errorf("the worker still runs: %v", h.f.Containers)
	}
	if h.state(t).Databases["main"] == nil {
		t.Error("the database's password was forgotten")
	}
}

func TestApplyPortsDontClash(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, "[apps.a]\n[apps.b]\n", map[string]string{"a": "r/a@sha256:1", "b": "r/b@sha256:2"})
	s := h.state(t)
	if s.Apps["a"].ProxyPort == s.Apps["b"].ProxyPort {
		t.Errorf("both on %d", s.Apps["a"].ProxyPort)
	}
}

func TestApplySecretsReachTheApp(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"m\"", 1)
	_, err := h.a.Apply(context.Background(), Release{
		Spec: parse(t, toml), Images: map[string]string{"crm": "r/c@sha256:1"},
		Secrets: map[string]map[string]string{"crm": {"STRIPE_KEY": "sk_live"}},
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(h.releases[0].Env, "STRIPE_KEY=sk_live") {
		t.Errorf("env %v", h.releases[0].Env)
	}
}

func TestDestroyOnlyOrphans(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "r/c@sha256:1"})
	ctx := context.Background()
	backedUp := 0
	backup := func(Resource) error { backedUp++; return nil }
	if err := h.a.Destroy(ctx, h.f, "kasl", "main", backup, io.Discard); err == nil || !strings.Contains(err.Error(), "still in hakobu.toml") {
		t.Errorf("destroying a database in the file: %v", err)
	}
	h.deploy(t, "[apps.crm]\n", map[string]string{"crm": "r/c@sha256:1"})
	if err := h.a.Destroy(ctx, h.f, "kasl", "main", func(Resource) error { return errors.New("bucket down") }, io.Discard); err == nil || !strings.Contains(err.Error(), "nothing was destroyed") {
		t.Errorf("without a backup: %v", err)
	}
	if _, ok := h.f.DBs["kasl_main"]; !ok {
		t.Fatal("dropped without a backup")
	}
	for _, name := range []string{"main", "cache", "crm/uploads"} {
		if err := h.a.Destroy(ctx, h.f, "kasl", name, backup, io.Discard); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, ok := h.f.DBs["kasl_main"]; ok || backedUp != 3 {
		t.Errorf("dbs %v, %d backups", h.f.DBs, backedUp)
	}
	if rs, _ := h.a.Resources(); len(rs) != 0 {
		t.Errorf("left %v", rs)
	}
	if err := h.a.Destroy(ctx, h.f, "kasl", "main", backup, io.Discard); err == nil {
		t.Error("destroyed twice")
	}
}

func TestEnsureProxies(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "r/c@sha256:1"})
	h.f.Proxies = map[string]string{}
	if err := h.a.EnsureProxies(context.Background(), h.f); err != nil {
		t.Fatal(err)
	}
	if h.f.Proxies["kasl-crm"] == "" {
		t.Errorf("proxies %v", h.f.Proxies)
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{
		"":           "",
		"migrated\n": "migrated",
		"a\nb\n\n":   "b",
		"2026-10-22T14:57:57.082623881Z ERROR: relation \"x\" does not exist\n": `ERROR: relation "x" does not exist`,
		"2026/10/22 14:57:57 boom": "2026/10/22 14:57:57 boom",
	} {
		if got := lastLine(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// A deploy can take minutes (a pull, a long migration); the ingress, the
// status and the health checks read the state meanwhile and mustn't wait
// for it.
func TestReadsDontWaitForADeploy(t *testing.T) {
	h := newHarness(t)
	toml := strings.Replace(crm, `worker = "bun run worker"`, `worker = "bun run worker"`+"\nrelease = \"bun run migrate\"", 1)
	h.deploy(t, toml, map[string]string{"crm": "reg/crm@sha256:aaa"})

	migrating, done := make(chan struct{}), make(chan struct{})
	h.a.RunOnce = func(context.Context, string, string, string, deploy.AppOptions) (string, error) {
		close(migrating)
		<-done
		return "", nil
	}
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		_, _ = h.a.Apply(context.Background(), Release{Spec: parse(t, toml), Images: map[string]string{"crm": "reg/crm@sha256:bbb"}}, io.Discard)
	}()
	<-migrating
	defer func() { close(done); <-applied }() // the deploy ends before its files go

	read := make(chan map[string]int64)
	go func() {
		_, _ = h.a.Apps()
		_, _ = h.a.Specs()
		_, _ = h.a.Resources()
		read <- h.a.Domains()
	}()
	select {
	case d := <-read:
		if d["crm.kaslauto.com"] == 0 {
			t.Errorf("domains during the deploy: %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading the state waited for the deploy")
	}
}

// Forced, the same release starts every app anew: hakobu deploy --force is
// how a restart is asked for.
func TestApplyForcedRedeploys(t *testing.T) {
	h := newHarness(t)
	images := map[string]string{"crm": "reg/crm@sha256:aaa"}
	h.deploy(t, crm, images)
	slot := h.state(t).Apps["crm"].Slot
	p, err := h.a.Apply(context.Background(), Release{Spec: parse(t, crm), Images: images, Force: true}, io.Discard)
	if err != nil || p.Empty() || !slices.Equal(p.Redeploy, []string{"crm"}) {
		t.Fatalf("plan %v, %v", p, err)
	}
	if now := h.state(t).Apps["crm"].Slot; now == slot {
		t.Errorf("still on slot %s", now)
	}
}

// A domain another project on the server has is refused, before anything
// changes.
func TestApplyRefusesAnotherProjectsDomain(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "reg/crm@sha256:aaa"})
	other, err := spec.Parse([]byte("project = \"shop\"\nserver = \"kasl-1\"\n[apps.web]\ndomain = \"crm.kaslauto.com\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	before := len(h.f.Calls())
	_, err = h.a.Apply(context.Background(), Release{Spec: other, Images: map[string]string{"web": "reg/web@sha256:bbb"}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is project kasl's") {
		t.Fatalf("err %v", err)
	}
	if len(h.f.Calls()) != before {
		t.Error("it changed something first")
	}
}

func TestApplyRefusesAnotherProjectsNames(t *testing.T) {
	h := newHarness(t)
	first, err := spec.Parse([]byte("project = \"kasl-crm\"\nserver = \"kasl-1\"\n[apps.web]\nvolumes = { uploads = \"/data\" }\n[caches.jobs]\n[databases.main]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.Apply(context.Background(), Release{Spec: first, Images: map[string]string{"web": "reg/web@sha256:aaa"}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ toml, want string }{
		// kasl + crm-web is kasl-crm + web: its containers, proxy, volumes.
		{"[apps.crm-web]\n", "kasl's apps.crm-web would be named as kasl-crm's apps.web is"},
		{"[apps.web]\n[caches.crm-jobs]\n", "kasl's caches.crm-jobs would be named as kasl-crm's caches.jobs is"},
		{"[apps.web]\n[databases.crm-main]\n", "kasl's databases.crm-main would be named as kasl-crm's databases.main is"},
	} {
		other, err := spec.Parse([]byte("project = \"kasl\"\nserver = \"kasl-1\"\n" + tc.toml))
		if err != nil {
			t.Fatal(err)
		}
		before := len(h.f.Calls())
		_, err = h.a.Apply(context.Background(), Release{Spec: other, Images: map[string]string{"web": "reg/web@sha256:bbb", "crm-web": "reg/web@sha256:bbb"}}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err %v", tc.toml, err)
		}
		if len(h.f.Calls()) != before {
			t.Errorf("%q: it changed something first", tc.toml)
		}
	}
}

func TestApplyRefusesNamesOfItsOwnAndHakobus(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ toml, want string }{
		// The cache would be the app's worker container.
		{"project = \"shop\"\n[apps.web]\n[caches.web-worker]\n", "shop's caches.web-worker would be named as shop's apps.web is (shop-web-worker)"},
		// The cache would be the shared Postgres.
		{"project = \"hakobu\"\n[apps.web]\n[caches.postgres]\n", "hakobu's own"},
		{"project = \"hakobu-vol\"\n[apps.web]\n", "hakobu's own"},
	} {
		p, err := spec.Parse([]byte("server = \"kasl-1\"\n" + tc.toml))
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.a.Apply(context.Background(), Release{Spec: p, Images: map[string]string{"web": "reg/web@sha256:bbb"}}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err %v", tc.toml, err)
		}
	}
	if calls := h.f.Calls(); len(calls) != 0 {
		t.Errorf("it changed something: %v", calls)
	}
}

type reconciled struct{ apps []node.AppSpec }

func (r *reconciled) Reconcile(_ context.Context, apps []node.AppSpec) { r.apps = apps }

func TestReconcilePassesTheLiveApps(t *testing.T) {
	h := newHarness(t)
	h.deploy(t, crm, map[string]string{"crm": "reg/crm@sha256:aaa"})
	var r reconciled
	if err := h.a.Reconcile(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.apps) != 1 || r.apps[0].Name != "kasl-crm" || r.apps[0].ActiveSlot != h.state(t).Apps["crm"].Slot {
		t.Errorf("reconciled %+v", r.apps)
	}
}

// The server's own credentials for a registry win; else the token CI
// sealed in the release pulls from its registry, and from no other.
func TestAuthFor(t *testing.T) {
	ci := &PullAuth{Host: "ghcr.io", Username: "kasl", Password: "ghs_job"}
	r := Release{Pull: ci}
	a := &Applier{}
	if auth, fromCI := a.authFor(r, "ghcr.io/kasl/crm-web@sha256:"+strings.Repeat("a", 64)); !fromCI || auth.Password != "ghs_job" {
		t.Errorf("ghcr.io: %+v %v", auth, fromCI)
	}
	if auth, fromCI := a.authFor(r, "docker.io/library/redis@sha256:"+strings.Repeat("a", 64)); fromCI || auth.Password != "" {
		t.Errorf("another registry: %+v %v", auth, fromCI)
	}
	a.Auth = map[string]deploy.RegistryAuth{"ghcr.io": {Username: "bot", Password: "ghp_own"}}
	if auth, fromCI := a.authFor(r, "ghcr.io/kasl/crm-web@sha256:"+strings.Repeat("a", 64)); fromCI || auth.Password != "ghp_own" {
		t.Errorf("the server's own: %+v %v", auth, fromCI)
	}
}
