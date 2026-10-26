package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestGitHub(t *testing.T) {
	var got map[string]string
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	g := GitHub{API: srv.URL}
	err := g.Report(context.Background(), Commit{"https://github.com/nova/crm", sha, "ghp_x"}, Failure, strings.Repeat("x", 200))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/repos/nova/crm/statuses/"+sha || auth != "Bearer ghp_x" {
		t.Errorf("%s with %q", path, auth)
	}
	if got["state"] != "failure" || got["context"] != Context || len([]rune(got["description"])) != 140 {
		t.Errorf("sent %v", got)
	}
	if err := g.Report(context.Background(), Commit{"https://github.com/other/repo", sha, ""}, Success, ""); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("no token: %v", err)
	}
	if err := g.Report(context.Background(), Commit{"https://github.com/nova/crm", "abc", "t"}, Success, ""); err == nil {
		t.Error("a short SHA was taken")
	}
}

func TestGitHubRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	}))
	defer srv.Close()
	g := GitHub{API: srv.URL}
	err := g.Report(context.Background(), Commit{"https://github.com/nova/crm", sha, "t"}, Pending, "")
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v", err)
	}
}

func TestRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/nova/crm":     "nova/crm",
		"https://github.com/nova/crm.git": "nova/crm",
		"https://github.com/nova/crm/":    "nova/crm",
		" https://github.com/Nova/CRM ":   "Nova/CRM",
	} {
		if got, err := Repo(in); err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "https://gitlab.com/a/b", "https://github.com/a", "https://github.com/a/b/c"} {
		if _, err := Repo(in); !errors.Is(err, ErrNotGitHub) {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestGet(t *testing.T) {
	statuses := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/nova/crm/commits/"+sha+"/statuses" || r.Header.Get("Authorization") != "Bearer gh_t" {
			t.Errorf("%s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(statuses))
	}))
	defer srv.Close()
	g := GitHub{API: srv.URL}
	c := Commit{"https://github.com/nova/crm", sha, "gh_t"}
	if _, _, ok, err := g.Get(context.Background(), c); ok || err != nil {
		t.Errorf("none: ok %v, %v", ok, err)
	}
	statuses = `[{"state":"success","context":"ci/other"},{"state":"failure","context":"hakobu/deploy","description":"release failed"},{"state":"pending","context":"hakobu/deploy"}]`
	state, desc, ok, err := g.Get(context.Background(), c)
	if !ok || err != nil || state != Failure || desc != "release failed" {
		t.Errorf("got %s %q %v %v", state, desc, ok, err)
	}

	// Run again for the same commit: the last run's status isn't this one's.
	statuses = `[{"state":"success","context":"hakobu/deploy","created_at":"2026-10-26T10:39:46Z"}]`
	g.Since = time.Date(2026, 10, 26, 10, 58, 38, 0, time.UTC)
	if _, _, ok, err := g.Get(context.Background(), c); ok || err != nil {
		t.Errorf("an older status: ok %v, %v", ok, err)
	}
	statuses = `[{"state":"pending","context":"hakobu/deploy","created_at":"2026-10-26T10:59:47Z"},{"state":"success","context":"hakobu/deploy","created_at":"2026-10-26T10:39:46Z"}]`
	if state, _, ok, _ := g.Get(context.Background(), c); !ok || state != Pending {
		t.Errorf("a newer status: %s %v", state, ok)
	}
}
