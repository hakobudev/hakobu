package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

// withStdin runs f with stdin reading input, as under ssh without -t.
func withStdin(t *testing.T, input string, f func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()
	f()
}

func TestReadSecretWithoutTerminal(t *testing.T) {
	withStdin(t, "tok-123\n", func() {
		if got, err := readSecret(); err != nil || got != "tok-123\n" {
			t.Errorf("readSecret() = %q, %v; want the piped line", got, err)
		}
	})
	withStdin(t, "", func() {
		if _, err := readSecret(); !errors.Is(err, errNoSecret) {
			t.Errorf("readSecret() on empty stdin = %v, want errNoSecret", err)
		}
	})
}

// A rerun after a setup that failed on a deleted token asks for a new one
// instead of trying the saved one again; a working one is kept.
func TestSetupReplacesADeadSavedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
			return
		}
		switch r.URL.Path {
		case "/zones":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"z1","name":"example.com","account":{"id":"acc"}}]}`)
		default:
			fmt.Fprint(w, `{"success":true,"result":[]}`)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })

	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx, "dead"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", "new")
	if err := ensureCloudflare(s); err != nil {
		t.Fatal(err)
	}
	if cf, _ := s.GetCloudflare(ctx); string(cf.ApiToken) != "new" {
		t.Errorf("saved token %q, want the new one", cf.ApiToken)
	}
	t.Setenv("CLOUDFLARE_API_TOKEN", "other")
	if err := ensureCloudflare(s); err != nil {
		t.Fatal(err)
	}
	if cf, _ := s.GetCloudflare(ctx); string(cf.ApiToken) != "new" {
		t.Errorf("saved token %q, want the working one kept", cf.ApiToken)
	}
}
