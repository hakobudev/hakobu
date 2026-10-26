package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/node/nodetest"
)

// A server as server create leaves it: name, address, keys; the rest is
// for the API to set.
func bareConfig(dataDir string) string {
	return fmt.Sprintf(`
name = "kasl-2"
listen = "127.0.0.1:7070"
data_dir = %q
owner_key = %q
operator_token_sha256 = %q
admin_token_sha256 = %q
releases = ["ghcr.io/kasl/site:main"]
`, dataDir, owner.Recipient().String(), HashToken("op-token"), HashToken("admin-token"))
}

func newBareDaemon(t *testing.T) (*Daemon, *httptest.Server) {
	dir := t.TempDir()
	c, err := ParseConfig([]byte(bareConfig(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitKey(c); err != nil {
		t.Fatal(err)
	}
	d, err := New(c, machine{Fake: nodetest.New()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.API())
	t.Cleanup(srv.Close)
	return d, srv
}

// fakeBucket is an S3 bucket that keeps what's put, path-style.
func fakeBucket(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	objects := map[string][]byte{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = b
		case "GET":
			b, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		case "DELETE":
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSettingsMerge(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{
		Releases:   []string{"ghcr.io/kasl/crm:main", "ghcr.io/kasl/site:main"},
		Registries: map[string]Credentials{"ghcr.io": {Username: "api", Password: "x"}, "quay.io": {Username: "robot", Password: "y"}},
		Updates:    &Updates{Auto: "none"},
		Pulse:      "https://hc-ping.com/abc",
	}
	c, err := ParseConfigWith([]byte(configText(dir)), s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Releases, " ") != "ghcr.io/kasl/crm-release:main ghcr.io/kasl/crm:main ghcr.io/kasl/site:main" {
		t.Errorf("releases %v", c.Releases)
	}
	if c.Registries["ghcr.io"].Username != "bot" || c.Registries["quay.io"].Username != "robot" {
		t.Errorf("server.toml's registry should win, the API's other be added: %v", c.Registries)
	}
	if c.Updates.Auto != "none" || c.Updates.At != "04:00" || c.Alerts.Pulse != "https://hc-ping.com/abc" {
		t.Errorf("updates %+v, pulse %q", c.Updates, c.Alerts.Pulse)
	}
	// A bad setting stops the config, as a bad server.toml does.
	if _, err := ParseConfigWith([]byte(configText(dir)), &Settings{Updates: &Updates{Auto: "always"}}); err == nil {
		t.Error("took updates.auto = always")
	}
}

func TestAPISettings(t *testing.T) {
	d, srv := newBareDaemon(t)
	post := func(token, body string) (int, map[string]any) {
		return call(t, srv, token, "POST", "/v1/settings", body)
	}

	if code, body := post("op-token", `{"add_release":"ghcr.io/kasl/crm:main"}`); code != http.StatusOK || body["rev"] != 1.0 {
		t.Fatalf("add a release: %d %v", code, body)
	}
	select {
	case <-d.restart:
	default:
		t.Error("no restart asked for")
	}
	if code, body := post("op-token", `{"remove_release":"ghcr.io/kasl/site:main"}`); code != http.StatusConflict || !strings.Contains(fmt.Sprint(body), "server.toml") {
		t.Errorf("remove server.toml's release: %d %v", code, body)
	}
	if code, body := post("op-token", `{"updates":{"auto":"sometimes"}}`); code != http.StatusBadRequest {
		t.Errorf("bad updates: %d %v", code, body)
	}
	if code, _ := post("op-token", `{"registry":{"host":"ghcr.io","username":"bot","password":"p4ss"}}`); code != http.StatusOK {
		t.Errorf("a registry: %d", code)
	}

	// Mail that can't be sent isn't kept.
	if code, body := post("op-token", `{"smtp":{"host":"127.0.0.1","port":1,"from":"a@b.c","to":"d@e.f"}}`); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body), "mail server") {
		t.Errorf("smtp that fails: %d %v", code, body)
	}

	// A bucket is the admin's, and proved before it's kept.
	bucket := fakeBucket(t)
	b := fmt.Sprintf(`{"backups":{"bucket":{"endpoint":"http://%s","region":"auto","bucket":"b","access_key":"k","secret_key":"s3cret"}}}`, strings.TrimPrefix(bucket.URL, "http://"))
	if code, body := post("op-token", b); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "admin") {
		t.Errorf("a bucket with the operator token: %d %v", code, body)
	}
	if code, body := post("admin-token", b); code != http.StatusOK || body["rev"] != 3.0 {
		t.Errorf("a bucket: %d %v", code, body)
	}

	s, err := LoadSettings(d.Config.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if s.Rev != 3 || len(s.Releases) != 1 || s.Registries["ghcr.io"].Password != "p4ss" || s.SMTP != nil ||
		s.Backups == nil || s.Backups.Primary == nil || s.Backups.Primary.Prefix != "hakobu/kasl-2/" {
		t.Errorf("settings %+v", s)
	}
	if fi, _ := os.Stat(d.Config.SettingsFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings.toml is %v", fi.Mode())
	}

	// What it shows has no password or secret key.
	code, view := call(t, srv, "op-token", "GET", "/v1/settings", "")
	if code != http.StatusOK || strings.Contains(fmt.Sprint(view), "p4ss") || strings.Contains(fmt.Sprint(view), "s3cret") {
		t.Errorf("settings: %d %v", code, view)
	}

	// A release that deployed a project is the admin's to remove: another
	// image may deploy the project then.
	if err := os.WriteFile(d.Config.WatchFile(), []byte(`{"ghcr.io/kasl/crm:main":{"project":"crm"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := post("op-token", `{"remove_release":"ghcr.io/kasl/crm:main"}`); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "deploys project crm") {
		t.Errorf("remove a project's release with the operator token: %d %v", code, body)
	}
	if code, body := post("admin-token", `{"remove_release":"ghcr.io/kasl/crm:main"}`); code != http.StatusOK {
		t.Errorf("remove a project's release: %d %v", code, body)
	}
	if b, _ := os.ReadFile(d.Config.WatchFile()); strings.Contains(string(b), "crm") {
		t.Errorf("watch.json still has it: %s", b)
	}
}

// A restart waits for the backups to finish.
func TestRestartWaitsForBackups(t *testing.T) {
	d, _ := newBareDaemon(t)
	d.backingUp = true
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.restartWhenAsked(ctx, func() { close(stopped) })
	d.restart <- struct{}{}
	select {
	case <-stopped:
		t.Fatal("restarted during the backups")
	case <-time.After(100 * time.Millisecond):
	}
	d.mu.Lock()
	d.backingUp = false
	d.mu.Unlock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("never restarted")
	}
	if !d.restarting {
		t.Error("not marked restarting")
	}
}
