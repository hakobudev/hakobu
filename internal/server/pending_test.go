package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/hakobudev/hakobu/internal/seal"
)

func TestPendingSecrets(t *testing.T) {
	d, srv := newBareDaemon(t)
	id, err := seal.LoadIdentity(d.Config.KeyFile())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := seal.New(id.Recipient().String(), owner.Recipient().String())
	sealed := func(v string) string { x, _ := s.Seal(v); return x }
	add := func(project, app, name, value string) int {
		b, _ := json.Marshal(map[string]string{"project": project, "app": app, "name": name, "sealed": value})
		code, _ := call(t, srv, "op-token", "POST", "/v1/secrets", string(b))
		return code
	}
	if code := add("kasl", "web", "DATABASE_URL", sealed("one")); code != http.StatusOK {
		t.Fatalf("add: %d", code)
	}
	if code := add("kasl", "web", "DATABASE_URL", sealed("two")); code != http.StatusOK {
		t.Fatalf("again: %d", code)
	}
	_ = add("site", "web", "KEY", sealed("x"))
	other, _ := seal.New(owner.Recipient().String())
	notOurs, _ := other.Seal("x")
	for _, bad := range []struct{ project, app, name, sealed string }{
		{"kasl", "web", "DATABASE_URL", notOurs},
		{"kasl", "web", "lower", sealed("x")},
		{"kasl", "hakobu", "nope", sealed("x")},
		{"Kasl!", "web", "X", sealed("x")},
	} {
		if code := add(bad.project, bad.app, bad.name, bad.sealed); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}

	get := func(method, path string) []PendingSecret {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer op-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var ps []PendingSecret
		_ = json.NewDecoder(resp.Body).Decode(&ps)
		return ps
	}
	ps := get("GET", "/v1/secrets?project=kasl")
	if len(ps) != 1 {
		t.Fatalf("kasl's: %+v", ps)
	}
	if v, err := seal.OpenValue(ps[0].Sealed, owner); err != nil || v != "two" {
		t.Errorf("the newer one: %q %v", v, err)
	}
	if got := get("POST", "/v1/secrets/clear?project=kasl"); len(got) != 1 {
		t.Errorf("clear gave %+v", got)
	}
	if got := get("GET", "/v1/secrets?project=kasl"); len(got) != 0 {
		t.Errorf("after clear: %+v", got)
	}
	if got := get("GET", "/v1/secrets?project=site"); len(got) != 1 {
		t.Errorf("another project's went too: %s", fmt.Sprint(got))
	}
}
