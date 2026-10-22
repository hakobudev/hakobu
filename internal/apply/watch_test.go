package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/registry"
	"github.com/hakobudev/hakobu/internal/seal"
	"github.com/hakobudev/hakobu/internal/status"
)

// releases is a registry with one release image, whose labels a test sets.
type releases struct {
	*httptest.Server
	mu   sync.Mutex
	docs map[string][]byte
	tag  string
}

func newReleases(t *testing.T) *releases {
	r := &releases{docs: map[string][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		path := strings.Replace(req.URL.Path, "/manifests/main", "/manifests/"+r.tag, 1)
		doc, ok := r.docs[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", r.tag)
		_, _ = w.Write(doc)
	}))
	t.Cleanup(r.Close)
	return r
}

// publish makes "main" a release image with these labels.
func (r *releases) publish(labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	put := func(kind string, v any) string {
		b, _ := json.Marshal(v)
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		r.docs["/v2/org/kasl/"+kind+"/"+d] = b
		return d
	}
	cfg := put("blobs", map[string]any{"config": map[string]any{"Labels": labels}})
	r.tag = put("manifests", map[string]any{"config": map[string]string{"digest": cfg}})
}

func (r *releases) ref(t *testing.T) (*registry.Client, registry.Ref) {
	host := strings.TrimPrefix(r.URL, "http://")
	ref, err := registry.ParseRef(host + "/org/kasl:main")
	if err != nil {
		t.Fatal(err)
	}
	return &registry.Client{Insecure: map[string]bool{host: true}}, ref
}

// serverKey is the test server's age key; releases' secrets are sealed
// to it.
var serverKey, _ = seal.NewIdentity()

// sealed is a hakobu.secrets.toml with a status token and these secrets of
// crm.
func sealed(crmSecrets map[string]string) string {
	s, err := seal.New(serverKey.Recipient().String())
	if err != nil {
		panic(err)
	}
	_ = s.Set("hakobu", seal.StatusToken, "status-token")
	for k, v := range crmSecrets {
		_ = s.Set("crm", k, v)
	}
	b, _ := s.Marshal()
	return string(b)
}

func releaseLabels(toml string, images map[string]string) map[string]string {
	b, _ := json.Marshal(images)
	return map[string]string{
		LabelSpec: "project = \"kasl\"\nserver = \"kasl-1\"\n" + toml, LabelImages: string(b),
		LabelSource: "https://github.com/kasl/crm", LabelRevision: "abc123", LabelSecrets: sealed(nil),
	}
}

var crmImage = func(c string) map[string]string {
	return map[string]string{"crm": "ghcr.io/kasl/crm@sha256:" + strings.Repeat(c, 64)}
}

func newWatch(t *testing.T, h *harness, regs *releases) (*Watch, registry.Ref) {
	c, ref := regs.ref(t)
	dir := t.TempDir()
	h.a.Server = "kasl-1"
	return &Watch{
		Applier: h.a, Registry: c, Releases: []registry.Ref{ref}, Identity: serverKey,
		WatchFile: filepath.Join(dir, "watch.json"), DeploysFile: filepath.Join(dir, "deploys.jsonl"),
	}, ref
}

func TestWatchDeploysNewReleases(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	var started, done []string
	w.Started = func(m Meta) { started = append(started, m.Revision) }
	w.Done = func(m Meta, r Result) { done = append(done, m.Revision+" "+r.Error) }
	ctx := context.Background()

	regs.publish(releaseLabels(crm, crmImage("a")))
	res, err := w.Check(ctx, ref, false)
	if err != nil || res == nil || res.Error != "" || res.Project != "kasl" || res.Revision != "abc123" {
		t.Fatalf("first: %+v, %v", res, err)
	}
	if h.f.Proxies["kasl-crm"] == "" {
		t.Error("nothing went live")
	}
	// The same release again is nothing.
	if res, err := w.Check(ctx, ref, false); res != nil || err != nil {
		t.Errorf("the same release deployed again: %+v, %v", res, err)
	}
	regs.publish(releaseLabels(crm, crmImage("b")))
	if res, _ := w.Check(ctx, ref, false); res == nil || res.Error != "" {
		t.Errorf("second release: %+v", res)
	}
	if got := h.state(t).Apps["crm"].Image; !strings.HasSuffix(got, "bbbb") {
		t.Errorf("live image %s", got)
	}
	if len(started) != 2 || len(done) != 2 || done[0] != "abc123 " {
		t.Errorf("started %v, done %v", started, done)
	}
	history, err := Deploys(w.DeploysFile, 10)
	if err != nil || len(history) != 2 || !strings.Contains(strings.Join(history[1].Changes, "\n"), "+ app") {
		t.Errorf("history %+v, %v", history, err)
	}
}

func TestWatchDoesntRetryAFailure(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	ctx := context.Background()
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"m\"", 1)
	regs.publish(releaseLabels(toml, crmImage("a")))
	h.failRun = deploy.ErrReleaseFailed
	res, _ := w.Check(ctx, ref, false)
	if res == nil || !strings.Contains(res.Error, "release command") {
		t.Fatalf("result %+v", res)
	}
	if res, _ := w.Check(ctx, ref, false); res != nil {
		t.Errorf("a failed release was tried again: %+v", res)
	}
	h.failRun = nil
	if res, _ := w.Check(ctx, ref, true); res == nil || res.Error != "" {
		t.Errorf("forced: %+v", res)
	}
}

func TestWatchRollback(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	ctx := context.Background()
	if _, err := w.Rollback(ctx, ref); err == nil {
		t.Error("rolled back with nothing before")
	}
	for _, c := range []string{"a", "b"} {
		regs.publish(releaseLabels(crm, crmImage(c)))
		if res, err := w.Check(ctx, ref, false); err != nil || res.Error != "" {
			t.Fatalf("release %s: %+v, %v", c, res, err)
		}
	}

	res, err := w.Rollback(ctx, ref)
	if err != nil || res.Error != "" {
		t.Fatalf("rollback: %+v, %v", res, err)
	}
	if got := h.state(t).Apps["crm"].Image; !strings.HasSuffix(got, "aaaa") {
		t.Errorf("live image %s after the rollback", got)
	}
	// The release rolled back from stays back until a newer one.
	if res, _ := w.Check(ctx, ref, false); res != nil {
		t.Errorf("the rolled-back release came back: %+v", res)
	}
	regs.publish(releaseLabels(crm, crmImage("c")))
	if res, _ := w.Check(ctx, ref, false); res == nil || res.Error != "" {
		t.Errorf("a newer release: %+v", res)
	}
}

func TestWatchSecrets(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	ctx := context.Background()
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"m\"", 1)
	env := func() string { return strings.Join(h.releases[len(h.releases)-1].Env, "\n") }
	for _, c := range []string{"a", "b"} {
		l := releaseLabels(toml, crmImage(c))
		l[LabelSecrets] = sealed(map[string]string{"STRIPE_KEY": "key-" + c})
		regs.publish(l)
		if res, err := w.Check(ctx, ref, false); err != nil || res.Error != "" {
			t.Fatalf("release %s: %+v, %v", c, res, err)
		}
		if !strings.Contains(env(), "STRIPE_KEY=key-"+c) {
			t.Errorf("release %s: env\n%s", c, env())
		}
	}
	// Rolling back to a's code keeps b's secrets: a key replaced since a
	// mustn't come back.
	if res, err := w.Rollback(ctx, ref); err != nil || res.Error != "" {
		t.Fatalf("rollback: %+v, %v", res, err)
	}
	if !strings.Contains(env(), "STRIPE_KEY=key-b") {
		t.Errorf("after the rollback: env\n%s", env())
	}
}

func TestWatchRefuses(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"other server", func() map[string]string {
			l := releaseLabels(crm, crmImage("a"))
			l[LabelSpec] = strings.Replace(l[LabelSpec], "kasl-1", "kasl-2", 1)
			return l
		}(), "for server kasl-2"},
		{"missing image", releaseLabels(crm, map[string]string{}), "no image for it"},
		{"extra image", releaseLabels(crm, map[string]string{"crm": crmImage("a")["crm"], "admin": crmImage("b")["crm"]}), "image for admin"},
		{"tag not digest", releaseLabels(crm, map[string]string{"crm": "ghcr.io/kasl/crm:main"}), "pinned by digest"},
		{"bad toml", releaseLabels("[apps.crm]\nhelth = \"/\"", crmImage("a")), "unknown keys"},
		{"stranger's secrets", func() map[string]string {
			l := releaseLabels(crm, crmImage("a"))
			other, _ := seal.NewIdentity()
			s, _ := seal.New(other.Recipient().String())
			_ = s.Set("crm", "KEY", "v")
			b, _ := s.Marshal()
			l[LabelSecrets] = string(b)
			return l
		}(), "not sealed to this key"},
		{"secret clashes", func() map[string]string {
			l := releaseLabels(crm, crmImage("a"))
			l[LabelSecrets] = sealed(map[string]string{"NODE_ENV": "x"})
			return l
		}(), "apps.crm.NODE_ENV: hakobu.toml sets it already"},
		{"secret of no app", func() map[string]string {
			l := releaseLabels(crm, crmImage("a"))
			s, _ := seal.Parse([]byte(sealed(nil)))
			_ = s.Set("admin", "KEY", "v")
			b, _ := s.Marshal()
			l[LabelSecrets] = string(b)
			return l
		}(), "apps.admin: hakobu.toml has no such app"},
		{"no status token", func() map[string]string {
			l := releaseLabels(crm, crmImage("a"))
			delete(l, LabelSecrets)
			return l
		}(), "no token for the commit status"},
		{"not a release", map[string]string{"maintainer": "x"}, "not a release image"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, regs := newHarness(t), newReleases(t)
			w, ref := newWatch(t, h, regs)
			regs.publish(c.labels)
			res, err := w.Check(ctx, ref, false)
			if err != nil || res == nil || !strings.Contains(res.Error, c.want) {
				t.Errorf("result %+v, %v; want %q", res, err, c.want)
			}
			if len(h.f.Calls()) != 0 {
				t.Errorf("the node was touched: %v", h.f.Calls())
			}
		})
	}
}

type statuses []string

func (s *statuses) Report(_ context.Context, c status.Commit, st status.State, desc string) error {
	if c.Token != "status-token" {
		desc += " (token " + c.Token + ")"
	}
	*s = append(*s, string(st)+" "+desc)
	return nil
}

func TestWatchReportsStatuses(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	var got statuses
	w.ReportTo(&got)
	toml := strings.Replace(crm, "[apps.crm]", "[apps.crm]\nrelease = \"m\"", 1)
	regs.publish(releaseLabels(toml, crmImage("a")))
	if _, err := w.Check(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	h.failRun = deploy.ErrReleaseFailed
	regs.publish(releaseLabels(toml, crmImage("b")))
	if _, err := w.Check(context.Background(), ref, false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != "pending deploying" || !strings.HasPrefix(got[1], "success live") ||
		got[2] != "pending deploying" || !strings.HasPrefix(got[3], "failure ") || !strings.Contains(got[3], "release command") {
		t.Errorf("statuses %q", got)
	}
}

// The registry may have dropped old releases (a cleanup policy, ttl.sh) or
// be down: a rollback goes from the server's own copies.
func TestRollbackWithoutRegistry(t *testing.T) {
	h, regs := newHarness(t), newReleases(t)
	w, ref := newWatch(t, h, regs)
	ctx := context.Background()
	for _, c := range []string{"a", "b"} {
		regs.publish(releaseLabels(crm, crmImage(c)))
		if res, err := w.Check(ctx, ref, false); err != nil || res.Error != "" {
			t.Fatalf("release %s: %+v, %v", c, res, err)
		}
	}
	regs.mu.Lock()
	regs.docs = map[string][]byte{}
	regs.mu.Unlock()
	res, err := w.Rollback(ctx, ref)
	if err != nil || res.Error != "" {
		t.Fatalf("rollback: %+v, %v", res, err)
	}
	if got := h.state(t).Apps["crm"].Image; !strings.HasSuffix(got, "aaaa") {
		t.Errorf("live %s", got)
	}
}

func TestShort(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	for in, want := range map[string]string{d: "abababababab", "ttl.sh/x-web@" + d: "abababababab", "": "none"} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q", in, got)
		}
	}
}
