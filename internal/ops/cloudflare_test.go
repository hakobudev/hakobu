package ops

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/store"
)

// A fresh panel has no account saved yet, so a refused tunnel is checked
// against the domain's account: an account's token there is valid and
// lacks a permission, not "deleted".
func TestSetupTunnelRefusalAsksTheDomainsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/zones":
			fmt.Fprint(w, `{"success":true,"result":[{"id":"z1","name":"example.com","account":{"id":"acc"}}]}`)
		case r.URL.Path == "/accounts/acc/tokens/verify":
			fmt.Fprint(w, `{"success":true,"result":{"id":"t","status":"active"}}`)
		case r.URL.Path == "/user/tokens/verify":
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`)
		case r.Method == "POST" && r.URL.Path == "/accounts/acc/cfd_tunnel":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })

	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx(), "tok"); err != nil {
		t.Fatal(err)
	}
	_, err = SetupTunnel(s, "z1", "hakobu")
	if err == nil || !strings.Contains(err.Error(), cloudflare.TokenLacksPermission) {
		t.Errorf("SetupTunnel: %v, want %q", err, cloudflare.TokenLacksPermission)
	}
}
