package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

	d.latest = "v1.3.0"
	_, body := call(t, srv, "op-token", "GET", "/v1/status", "")
	if body["version"] != "v1.2.0" || body["latest"] != "v1.3.0" || body["last_update"].(map[string]any)["state"] != "running" {
		t.Errorf("status %v", body)
	}
	d.latest = "v1.2.0"
	if _, body := call(t, srv, "op-token", "GET", "/v1/status", ""); body["latest"] != nil {
		t.Errorf("an older latest shows: %v", body["latest"])
	}
}
