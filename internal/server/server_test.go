package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
)

var owner, _ = age.GenerateX25519Identity()

func configText(dataDir string) string {
	return fmt.Sprintf(`
name = "nova-1"
listen = "100.64.0.1:7070"
data_dir = %q
owner_key = %q
operator_token_sha256 = %q
admin_token_sha256 = %q
releases = ["ghcr.io/nova/crm-release:main"]
[registries."ghcr.io"]
username = "bot"
password = "pat"
[backups.primary]
name = "r2"
endpoint = "https://acc.r2.cloudflarestorage.com"
bucket = "nova-backups"
access_key = "k"
secret_key = "s"
[[backups.copies]]
name = "hetzner"
endpoint = "https://fsn1.your-objectstorage.com"
bucket = "nova-copy"
access_key = "k2"
secret_key = "s2"
`, dataDir, owner.Recipient().String(), HashToken("op-token"), HashToken("admin-token"))
}

func TestConfig(t *testing.T) {
	c, err := ParseConfig([]byte(configText("/tmp/x")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Backups.Primary.Prefix != "hakobu/nova-1/" || c.Backups.Copies[0].Prefix != "hakobu/nova-1/" {
		t.Errorf("prefixes %q %q", c.Backups.Primary.Prefix, c.Backups.Copies[0].Prefix)
	}
	if time.Duration(c.Every) != 30*time.Second || c.Backups.At != "03:00" || c.Backups.Daily != 7 || c.Backups.Weekly != 4 {
		t.Errorf("defaults %+v", c)
	}
	for token, want := range map[string]Level{"op-token": Operator, "admin-token": Admin, "": None, "other": None} {
		if got := c.TokenLevel(token); got != want {
			t.Errorf("%q is %v, want %v", token, got, want)
		}
	}
}

func TestConfigRefuses(t *testing.T) {
	good := configText("/tmp/x")
	for _, c := range []struct{ name, from, to, want string }{
		{"public listen", `listen = "100.64.0.1:7070"`, `listen = "0.0.0.0:7070"`, "every address"},
		{"public listen v6", `listen = "100.64.0.1:7070"`, `listen = "[::]:7070"`, "every address"},
		{"no port", `listen = "100.64.0.1:7070"`, `listen = "100.64.0.1"`, "host:port"},
		{"a name", `listen = "100.64.0.1:7070"`, `listen = "example.com:7070"`, "not a name"},
		{"a public address", `listen = "100.64.0.1:7070"`, `listen = "203.0.113.5:7070"`, "isn't loopback or on a tailnet"},
		{"no listen", `listen = "100.64.0.1:7070"`, `listen = ""`, "tailnet"},
		{"owner key", `owner_key = "age1`, `owner_key = "ssh-ed25519 age1`, "owner_key"},
		{"pinned release", `crm-release:main"`, `crm-release@sha256:` + strings.Repeat("a", 64) + `"`, "would never change"},
		{"http bucket", `"https://acc.r2`, `"http://acc.r2`, "should be https"},
		{"typo", `[backups.primary]`, "keep_dayly = 3\n[backups.primary]", "unknown keys"},
		{"bad time", `[backups.primary]`, "[backups]\nat = \"3am\"\n[backups.primary]", "time of day"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(strings.Replace(good, c.from, c.to, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestNextAt(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	if got := nextAt(now, "03:00"); !got.Equal(time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("next = %v", got)
	}
	if got := nextAt(now, "23:30"); !got.Equal(time.Date(2026, 10, 22, 23, 30, 0, 0, time.UTC)) {
		t.Errorf("next = %v", got)
	}
}

// machine is the fake node with the few calls the daemon adds.
type machine struct {
	*nodetest.Fake
	used, total uint64
}

func (m machine) Disk(context.Context) (uint64, uint64, error) { return m.used, m.total, nil }

func (machine) Logs(context.Context, string, int) (string, error) { return "listening on :8080", nil }
func (machine) CheckDump(context.Context, node.DBSpec, io.Reader) (int, error) {
	return 1, nil
}
func (machine) LoadDump(context.Context, node.DBSpec, io.Reader) error { return nil }
func (machine) ArchiveVolume(context.Context, string, string, io.Writer) error {
	return nil
}
func (machine) CheckArchive(io.Reader) (int, error) { return 0, nil }
func (machine) LoadArchive(context.Context, node.AppSpec, string, io.Reader, io.Writer) error {
	return nil
}

func newDaemon(t *testing.T) (*Daemon, *httptest.Server) {
	dir := t.TempDir()
	c, err := ParseConfig([]byte(configText(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(c, machine{Fake: nodetest.New()}, io.Discard); err == nil {
		t.Fatal("started without a key")
	}
	recipient, err := InitKey(c)
	if err != nil || !strings.HasPrefix(recipient, "age1") {
		t.Fatalf("InitKey: %s, %v", recipient, err)
	}
	if again, _ := InitKey(c); again != recipient {
		t.Fatal("InitKey replaced the key")
	}
	d, err := New(c, machine{Fake: nodetest.New()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.API())
	t.Cleanup(srv.Close)
	return d, srv
}

func call(t *testing.T, srv *httptest.Server, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestAPIAuth(t *testing.T) {
	_, srv := newDaemon(t)
	if code, _ := call(t, srv, "", "GET", "/v1/status", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := call(t, srv, "wrong", "GET", "/v1/status", ""); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code, body := call(t, srv, "op-token", "GET", "/v1/status", ""); code != http.StatusOK || body["server"] != "nova-1" || body["backups"] != true {
		t.Errorf("status: %d %v", code, body)
	}
	for _, path := range []string{"/v1/restore", "/v1/destroy"} {
		if code, body := call(t, srv, "op-token", "POST", path, "{}"); code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body), "admin") {
			t.Errorf("%s with the operator token: %d %v", path, code, body)
		}
	}
}

func TestAPIKey(t *testing.T) {
	_, srv := newDaemon(t)
	code, body := call(t, srv, "op-token", "GET", "/v1/key", "")
	if code != http.StatusOK || !strings.HasPrefix(fmt.Sprint(body["recipient"]), "age1") || body["owner"] != owner.Recipient().String() {
		t.Errorf("%d %v", code, body)
	}
}

func TestAPIDestroyNeedsConfirm(t *testing.T) {
	_, srv := newDaemon(t)
	code, body := call(t, srv, "admin-token", "POST", "/v1/destroy", `{"project":"nova","name":"main","confirm":"nova/mian"}`)
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(body["error"]), "repeat nova/main") {
		t.Errorf("%d %v", code, body)
	}
	code, body = call(t, srv, "admin-token", "POST", "/v1/destroy", `{"project":"nova","name":"main","confirm":"nova/main"}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "no project nova") {
		t.Errorf("%d %v", code, body)
	}
}

func TestAPILogsOfNothing(t *testing.T) {
	_, srv := newDaemon(t)
	if code, _ := call(t, srv, "op-token", "GET", "/v1/logs?project=nova&app=crm", ""); code != http.StatusNotFound {
		t.Errorf("%d", code)
	}
}

// A failed deploy of a release image the config has since dropped closes as
// hakobu starts; one of an image it still has stays open.
func TestGoneReleasesAlertsClose(t *testing.T) {
	d, _ := newDaemon(t)
	keep := d.Watch.Releases[0]
	_ = d.Alerts.Raise("deploy:"+keep.Host+"/"+keep.Repo, "deploy of nova failed", "")
	_ = d.Alerts.Raise("deploy:ttl.sh/gone", "deploy of gone failed", "")
	d.closeGoneReleases()
	keys, _ := d.Alerts.Keys("deploy:")
	if len(keys) != 1 || keys[0] != "deploy:"+keep.Host+"/"+keep.Repo {
		t.Errorf("open: %v", keys)
	}
}
