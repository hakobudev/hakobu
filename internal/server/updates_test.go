package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/update"
)

func TestAPIUpdate(t *testing.T) {
	d, srv := newDaemon(t)
	d.Version, d.InstallDir = "1.2.0", t.TempDir()
	old := UpdateUnit
	UpdateUnit = filepath.Join(t.TempDir(), "hakobu-update.path")
	t.Cleanup(func() { UpdateUnit = old })
	request := filepath.Join(d.Config.DataDir, update.RequestFile)

	if code, body := call(t, srv, "op-token", "POST", "/v1/update", `{"to":"v1.3.0"}`); code != http.StatusConflict {
		t.Errorf("without the updater's unit: %d %v", code, body)
	}
	_ = os.WriteFile(UpdateUnit, nil, 0o644)
	for _, to := range []string{"v1.1.0", "v1.2.0", "nonsense"} {
		if code, body := call(t, srv, "op-token", "POST", "/v1/update", `{"to":"`+to+`"}`); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", to, code, body)
		}
	}
	if _, err := os.Stat(request); err == nil {
		t.Fatal("a refused update asked root")
	}

	if code, body := call(t, srv, "op-token", "POST", "/v1/update", `{"to":"1.3.0"}`); code != http.StatusOK || body["to"] != "v1.3.0" || body["from"] != "v1.2.0" {
		t.Errorf("%d %v", code, body)
	}
	if b, _ := os.ReadFile(request); string(b) != "v1.3.0" {
		t.Errorf("request %q", b)
	}
	if code, body := call(t, srv, "op-token", "POST", "/v1/update", `{}`); code != http.StatusOK || body["to"] != "latest" {
		t.Errorf("latest: %d %v", code, body)
	}
	if b, _ := os.ReadFile(request); string(b) != "" {
		t.Errorf("request for the latest %q", b)
	}

	// One running already.
	b, _ := json.Marshal(update.Status{At: time.Now(), From: "v1.2.0", To: "v1.3.0", State: "running"})
	_ = os.WriteFile(filepath.Join(d.InstallDir, update.StatusFile), b, 0o644)
	if code, body := call(t, srv, "op-token", "POST", "/v1/update", `{}`); code != http.StatusConflict {
		t.Errorf("while one runs: %d %v", code, body)
	}

	d.latest = update.Release{Tag: "v1.3.0"}
	_, body := call(t, srv, "op-token", "GET", "/v1/status", "")
	if body["version"] != "v1.2.0" || body["latest"] != "v1.3.0" || body["last_update"].(map[string]any)["state"] != "running" {
		t.Errorf("status %v", body)
	}
	d.latest = update.Release{Tag: "v1.2.0"}
	if _, body := call(t, srv, "op-token", "GET", "/v1/status", ""); body["latest"] != nil {
		t.Errorf("an older latest shows: %v", body["latest"])
	}
}

func TestUpdatesConfig(t *testing.T) {
	c, err := ParseConfig([]byte(configText("/tmp/x")))
	if err != nil || c.Updates.Auto != update.AutoPatch || c.Updates.At != "04:00" {
		t.Fatalf("defaults %+v, %v", c.Updates, err)
	}
	late := strings.Replace(configText("/tmp/x"), "[backups.primary]", "[backups]\nat = \"23:30\"\n[backups.primary]", 1)
	if c, err := ParseConfig([]byte(late)); err != nil || c.Updates.At != "00:30" {
		t.Errorf("after 23:30 backups: %+v, %v", c.Updates, err)
	}
	if _, err := ParseConfig([]byte(configText("/tmp/x") + "\n[updates]\nauto = \"always\"\n")); err == nil || !strings.Contains(err.Error(), "updates.auto") {
		t.Errorf("auto = always: %v", err)
	}
}

func TestAutoUpdate(t *testing.T) {
	d, _ := newDaemon(t)
	d.Version, d.InstallDir = "1.2.0", t.TempDir()
	old := UpdateUnit
	UpdateUnit = filepath.Join(t.TempDir(), "hakobu-update.path")
	_ = os.WriteFile(UpdateUnit, nil, 0o644)
	t.Cleanup(func() { UpdateUnit = old })
	latest := update.Release{Tag: "v1.2.1"}
	d.LatestRelease = func() (update.Release, error) { return latest, nil }
	request := filepath.Join(d.Config.DataDir, update.RequestFile)
	asked := func() string {
		b, err := os.ReadFile(request)
		os.Remove(request)
		if err != nil {
			return "none"
		}
		return string(b)
	}

	if msg := d.autoUpdate(); asked() != "v1.2.1" {
		t.Errorf("a patch: %q", msg)
	}
	latest.Tag = "v1.3.0"
	if msg := d.autoUpdate(); msg != "" || asked() != "none" {
		t.Errorf("a minor release under auto = patch: %q", msg)
	}
	d.Config.Updates.Auto = update.AutoMinor
	d.mu.Lock()
	d.backingUp = true
	d.mu.Unlock()
	if msg := d.autoUpdate(); !strings.Contains(msg, "tomorrow") || asked() != "none" {
		t.Errorf("during the backups: %q", msg)
	}
	d.mu.Lock()
	d.backingUp = false
	d.mu.Unlock()
	writeStatus(t, d, update.Status{At: time.Now(), From: "v1.2.0", To: "v1.3.0", State: "rolled back", Message: "didn't come up"})
	if msg := d.autoUpdate(); !strings.Contains(msg, "not trying again") || asked() != "none" {
		t.Errorf("after it rolled back: %q", msg)
	}
	latest.Tag = "v1.3.1"
	if msg := d.autoUpdate(); asked() != "v1.3.1" {
		t.Errorf("a newer one after: %q", msg)
	}
}

func writeStatus(t *testing.T, d *Daemon, st update.Status) {
	t.Helper()
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(d.InstallDir, update.StatusFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAlerts(t *testing.T) {
	d, _ := newDaemon(t)
	d.Version, d.InstallDir = "1.2.0", t.TempDir()
	open := func() []string {
		ps, _ := d.Alerts.Open()
		var out []string
		for _, p := range ps {
			out = append(out, p.Subject)
		}
		return out
	}
	writeStatus(t, d, update.Status{At: time.Now(), From: "v1.2.0", To: "v1.3.0", State: "rolled back", Message: "didn't come up"})
	d.latest = update.Release{Tag: "v1.3.0", Published: time.Now().Add(-20 * 24 * time.Hour)}
	d.updateAlerts()
	if got := open(); len(got) != 2 || !slices.Contains(got, "hakobu couldn't update to v1.3.0") || !slices.Contains(got, "hakobu v1.3.0 has been out for 20 days") {
		t.Errorf("open %v", got)
	}
	// Updated: both close.
	d.Version = "1.3.0"
	writeStatus(t, d, update.Status{At: time.Now(), From: "v1.2.0", To: "v1.3.0", State: "updated"})
	d.updateAlerts()
	if got := open(); len(got) != 0 {
		t.Errorf("after the update %v", got)
	}
	// A refused request for an older one, a release out a few days: nothing.
	writeStatus(t, d, update.Status{At: time.Now(), From: "v1.3.0", To: "v1.2.0", State: "failed"})
	d.latest = update.Release{Tag: "v1.4.0", Published: time.Now().Add(-3 * 24 * time.Hour)}
	d.updateAlerts()
	if got := open(); len(got) != 0 {
		t.Errorf("nothing to say %v", got)
	}
}
