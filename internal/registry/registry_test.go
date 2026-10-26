package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseRef(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for in, want := range map[string]Ref{
		"ghcr.io/org/kasl:main":     {Host: "ghcr.io", Repo: "org/kasl", Tag: "main"},
		"ghcr.io/org/kasl@" + d:     {Host: "ghcr.io", Repo: "org/kasl", Digest: d},
		"ghcr.io/org/kasl:v1@" + d:  {Host: "ghcr.io", Repo: "org/kasl", Tag: "v1", Digest: d},
		"busybox:1.36":              {Host: "registry-1.docker.io", Repo: "library/busybox", Tag: "1.36"},
		"user/app":                  {Host: "registry-1.docker.io", Repo: "user/app", Tag: "latest"},
		"localhost:5000/app:dev":    {Host: "localhost:5000", Repo: "app", Tag: "dev"},
		"reg.example.com:443/a/b/c": {Host: "reg.example.com:443", Repo: "a/b/c", Tag: "latest"},
	} {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("%s = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"ghcr.io/Org/app", "app@sha256:abc", "ghcr.io/"} {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	kind, p := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:org/app:pull"`)
	if kind != "bearer" || p["realm"] != "https://ghcr.io/token" || p["service"] != "ghcr.io" || p["scope"] != "repository:org/app:pull" {
		t.Errorf("%s %v", kind, p)
	}
	if kind, p := parseChallenge(`Basic realm="Registry"`); kind != "basic" || p["realm"] != "Registry" {
		t.Errorf("%s %v", kind, p)
	}
}

// fakeRegistry serves one repository, behind a bearer token it hands out
// only for the right password, with an index of a manifest for this
// machine, an attestation, and a config with labels.
type fakeRegistry struct {
	*httptest.Server
	heads, gets atomic.Int32
	tag         string // the digest "main" points at
}

func newFakeRegistry(t *testing.T, labels map[string]string) *fakeRegistry {
	f := &fakeRegistry{}
	blob := func(v any) (string, []byte) {
		b, _ := json.Marshal(v)
		sum := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(sum[:]), b
	}
	cfgDigest, cfg := blob(map[string]any{"config": map[string]any{"Labels": labels}})
	manDigest, man := blob(map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": map[string]string{"digest": cfgDigest}})
	idxDigest, idx := blob(map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []any{
		map[string]any{"digest": "sha256:" + strings.Repeat("0", 64), "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
		map[string]any{"digest": manDigest, "platform": map[string]string{"os": "linux", "architecture": runtime.GOARCH}},
	}})
	f.tag = idxDigest
	docs := map[string][]byte{"/v2/org/kasl/manifests/main": idx, "/v2/org/kasl/manifests/" + idxDigest: idx, "/v2/org/kasl/manifests/" + manDigest: man, "/v2/org/kasl/blobs/" + cfgDigest: cfg}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if u, p, _ := r.BasicAuth(); u != "bot" || p != "pat" || r.URL.Query().Get("scope") != "repository:org/kasl:pull" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"token":"tok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, f.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		doc, ok := docs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == "HEAD" {
			f.heads.Add(1)
		} else {
			f.gets.Add(1)
		}
		w.Header().Set("Docker-Content-Digest", idxDigest)
		_, _ = w.Write(doc)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRegistry) client(user, pass string) (*Client, Ref) {
	host := strings.TrimPrefix(f.URL, "http://")
	c := &Client{Insecure: map[string]bool{host: true}, Auth: map[string]Credentials{host: {user, pass}}}
	r, _ := ParseRef(host + "/org/kasl:main")
	return c, r
}

func TestDigestAndLabels(t *testing.T) {
	f := newFakeRegistry(t, map[string]string{"dev.hakobu.spec": "project = \"kasl\""})
	c, r := f.client("bot", "pat")
	ctx := context.Background()
	d, err := c.Digest(ctx, r)
	if err != nil || d != f.tag {
		t.Fatalf("digest %s, %v", d, err)
	}
	if f.gets.Load() != 0 || f.heads.Load() != 1 {
		t.Errorf("a poll took %d GETs and %d HEADs", f.gets.Load(), f.heads.Load())
	}
	// The token is kept: a second poll is one request.
	if _, err := c.Digest(ctx, r); err != nil || f.heads.Load() != 2 {
		t.Errorf("second poll: %v, %d heads", err, f.heads.Load())
	}
	labels, err := c.Labels(ctx, r.Pinned(d))
	if err != nil || labels["dev.hakobu.spec"] != `project = "kasl"` {
		t.Errorf("labels %v, %v", labels, err)
	}
}

func TestWrongPassword(t *testing.T) {
	f := newFakeRegistry(t, nil)
	c, r := f.client("bot", "wrong")
	if _, err := c.Digest(context.Background(), r); err == nil || !strings.Contains(err.Error(), "signing in") {
		t.Errorf("err = %v", err)
	}
}

func TestNotFound(t *testing.T) {
	f := newFakeRegistry(t, nil)
	c, r := f.client("bot", "pat")
	r.Tag = "nope"
	if _, err := c.Digest(context.Background(), r); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &Client{Insecure: map[string]bool{u.Host: true}, Auth: map[string]Credentials{u.Host: {"u", "p"}}}
	r, _ := ParseRef(u.Host + "/app:main")
	if d, err := c.Digest(context.Background(), r); err != nil || d != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("%s, %v", d, err)
	}
}

// A registry's digest names a file on the server: one that isn't a
// digest is refused, not followed out of the releases' folder.
func TestDigestRefusesNonDigests(t *testing.T) {
	for _, d := range []string{"sha256:../state", "sha256:" + strings.Repeat("A", 64), "sha512:" + strings.Repeat("a", 64)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Docker-Content-Digest", d)
		}))
		host := strings.TrimPrefix(srv.URL, "http://")
		c := &Client{Insecure: map[string]bool{host: true}}
		ref, _ := ParseRef(host + "/org/app:main")
		if got, err := c.Digest(context.Background(), ref); err == nil {
			t.Errorf("%s: got %s", d, got)
		}
		srv.Close()
	}
}

func TestLabelsRefusesLoopsAndNonDigests(t *testing.T) {
	self := "sha256:" + strings.Repeat("1", 64)
	for name, doc := range map[string]string{
		// An index naming itself would be read forever.
		"loop":   `{"manifests":[{"digest":"` + self + `","platform":{"os":"linux","architecture":"` + runtime.GOARCH + `"}}]}`,
		"index":  `{"manifests":[{"digest":"../../evil","platform":{"os":"linux","architecture":"` + runtime.GOARCH + `"}}]}`,
		"config": `{"config":{"digest":"../../evil"}}`,
	} {
		var gets atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			gets.Add(1)
			fmt.Fprint(w, doc)
		}))
		host := strings.TrimPrefix(srv.URL, "http://")
		c := &Client{Insecure: map[string]bool{host: true}}
		ref, _ := ParseRef(host + "/org/app@" + self)
		if _, err := c.Labels(context.Background(), ref); err == nil || gets.Load() > 2 {
			t.Errorf("%s: %v after %d requests", name, err, gets.Load())
		}
		srv.Close()
	}
}
