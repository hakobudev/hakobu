package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI is Tailscale's API for client "id"/"secret" owning tag:hakobu.
type fakeAPI struct {
	tokens  int
	keys    []map[string]any
	deleted []string
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "id" || r.Form.Get("client_secret") != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"invalid client"}`)
				return
			}
			f.tokens++
			fmt.Fprint(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch route := r.Method + " " + r.URL.Path; {
		case route == "POST /tailnet/-/keys":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			tags := body["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)["tags"].([]any)
			if len(tags) != 1 || tags[0] != "tag:hakobu" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"message":"requested tags are invalid or not permitted"}`)
				return
			}
			f.keys = append(f.keys, body)
			fmt.Fprint(w, `{"id":"k1","key":"tskey-auth-k1-abc"}`)
		case route == "GET /tailnet/-/devices":
			fmt.Fprint(w, `{"devices":[{"nodeId":"n1","hostname":"hakobu-kasl","addresses":["100.78.99.72","fd7a::1"],"tags":["tag:hakobu"],"connectedToControl":true}]}`)
		case route == "DELETE /device/n1":
			f.deleted = append(f.deleted, "n1")
		case strings.HasPrefix(route, "DELETE /device/"):
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"not found"}`)
		default:
			t.Errorf("unexpected %s", route)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := APIURL
	APIURL = srv.URL
	t.Cleanup(func() { APIURL = old })
	tokens.Lock()
	tokens.m = map[string]token{}
	tokens.Unlock()
	return f
}

func TestClient(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	c := Client{ID: "id", Secret: "secret", Tag: "tag:hakobu"}
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
	key, err := c.AuthKey(ctx, "hakobu kasl")
	if err != nil || key != "tskey-auth-k1-abc" {
		t.Fatalf("key = %q (%v)", key, err)
	}
	create := f.keys[1]["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["reusable"] != false || create["ephemeral"] != false || create["preauthorized"] != true {
		t.Errorf("key capabilities = %v", create)
	}
	devices, err := c.Devices(ctx)
	if err != nil || len(devices) != 1 || devices[0].IPv4() != "100.78.99.72" || !devices[0].Online {
		t.Fatalf("devices = %+v (%v)", devices, err)
	}
	if err := c.DeleteDevice(ctx, "n1"); err != nil || len(f.deleted) != 1 {
		t.Errorf("delete: %v, %v", err, f.deleted)
	}
	if err := c.DeleteDevice(ctx, "gone"); err != nil {
		t.Errorf("deleting a device already gone: %v", err)
	}
	if f.tokens != 1 {
		t.Errorf("%d tokens minted, want 1 reused", f.tokens)
	}
}

func TestCheckRefuses(t *testing.T) {
	newFake(t)
	ctx := context.Background()
	for _, c := range []Client{
		{ID: "id", Secret: "wrong", Tag: "tag:hakobu"},
		{ID: "id", Secret: "secret", Tag: "tag:other"},
		{ID: "id", Secret: "secret", Tag: "hakobu"},
	} {
		if err := c.Check(ctx); err == nil {
			t.Errorf("%+v passed", c)
		}
	}
}
