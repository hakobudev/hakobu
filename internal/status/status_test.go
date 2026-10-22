package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	g := GitHub{API: srv.URL, Token: func(repo string) string {
		if repo == "kasl/crm" {
			return "ghp_x"
		}
		return ""
	}}
	err := g.Report(context.Background(), "https://github.com/kasl/crm", sha, Failure, strings.Repeat("x", 200))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/repos/kasl/crm/statuses/"+sha || auth != "Bearer ghp_x" {
		t.Errorf("%s with %q", path, auth)
	}
	if got["state"] != "failure" || got["context"] != Context || len([]rune(got["description"])) != 140 {
		t.Errorf("sent %v", got)
	}
	if err := g.Report(context.Background(), "https://github.com/other/repo", sha, Success, ""); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("no token: %v", err)
	}
	if err := g.Report(context.Background(), "https://github.com/kasl/crm", "abc", Success, ""); err == nil {
		t.Error("a short SHA was taken")
	}
}

func TestGitHubRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	}))
	defer srv.Close()
	g := GitHub{API: srv.URL, Token: func(string) string { return "t" }}
	err := g.Report(context.Background(), "https://github.com/kasl/crm", sha, Pending, "")
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v", err)
	}
}

func TestRepo(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/kasl/crm":     "kasl/crm",
		"https://github.com/kasl/crm.git": "kasl/crm",
		"https://github.com/kasl/crm/":    "kasl/crm",
		" https://github.com/Kasl/CRM ":   "Kasl/CRM",
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
