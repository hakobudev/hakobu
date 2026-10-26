package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/client"
	"github.com/hakobudev/hakobu/internal/server"
)

// A change is saved, then in effect once the server answers with its
// rev, after the restart it went away for.
func TestSetSettingsWaits(t *testing.T) {
	settingsPoll = 10 * time.Millisecond
	t.Cleanup(func() { settingsPoll = 2 * time.Second })
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	var gets atomic.Int32
	var posted server.SettingsChange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin" {
			http.Error(w, `{"error":"no"}`, http.StatusForbidden)
			return
		}
		if r.Method == "POST" {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			_ = json.NewEncoder(w).Encode(map[string]int{"rev": 4})
			return
		}
		switch gets.Add(1) {
		case 1:
			_ = json.NewEncoder(w).Encode(server.SettingsView{Rev: 3}) // not yet
		case 2:
			http.Error(w, "restarting", http.StatusBadGateway)
		default:
			_ = json.NewEncoder(w).Encode(server.SettingsView{Rev: 4})
		}
	}))
	defer srv.Close()
	all := client.Servers{"kasl-2": {URL: srv.URL, Token: "op"}}
	if err := all.Save(filepath.Join(cfg, "hakobu", "servers.toml")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAKOBU_S3_SECRET_KEY", "s3cret")
	var out bytes.Buffer
	ch := server.SettingsChange{Backups: &server.BackupsSet{Bucket: &server.Bucket{Endpoint: "https://e", Bucket: "b", AccessKey: "k", SecretKey: os.Getenv("HAKOBU_S3_SECRET_KEY")}}}
	if err := setSettings(context.Background(), "kasl-2", "admin", ch, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "in effect") || gets.Load() != 3 || posted.Backups.Bucket.SecretKey != "s3cret" {
		t.Errorf("out %q, gets %d, posted %+v", out.String(), gets.Load(), posted.Backups.Bucket)
	}
}
