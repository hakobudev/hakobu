package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

// A telemetry.db hakobu can't use is replaced: one from a newer hakobu, or
// one sealed with a master key that's gone.
func TestTelemetryStartsOver(t *testing.T) {
	dir := t.TempDir()
	fill := func() {
		db, err := openTelemetry(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`INSERT INTO telemetry_events (app_id, kind) VALUES (1, 'error')`); err != nil {
			t.Fatal(err)
		}
	}
	empty := func(db *sql.DB, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM telemetry_events`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d events kept (err %v), want a new file", n, err)
		}
	}

	fill()
	db, err := sql.Open("sqlite", filepath.Join(dir, telemetryFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	empty(openTelemetry(dir, false))

	fill()
	empty(openTelemetry(dir, true))
}

func TestRotationReencryptsTelemetry(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: 1, Kind: "error", Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.telDB.QueryRow(`SELECT message FROM telemetry_events`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateMasterKey(); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := s.telDB.QueryRow(`SELECT message FROM telemetry_events`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("the telemetry is still sealed with the old key")
	}
	if plain, err := secret.Decrypt(after); err != nil || plain != "boom" {
		t.Errorf("after the rotation: %q, %v", plain, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, telemetryFile)); err != nil {
		t.Error(err)
	}
}

// Errors kept from before issues are grouped when hakobu next starts.
func TestOldEventsGetFingerprints(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hakobu.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for n := range 3 {
		// As migration 005 left them.
		payload := fmt.Sprintf(`{"logger":"src.phone.executor","logentry":{"message":"2026-10-14 09:%02d:07 | ERROR | UI dump: https://r2.example.com/%d.xml"}}`, n, n)
		if err := s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: 1, Kind: "error", Level: "error", Message: "UI dump", Payload: secret.String(payload), Fingerprint: fmt.Sprintf("event:%d", n+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Tel.ResolveIssue(ctx, teldb.ResolveIssueParams{ID: 1, AppID: 1}); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	s.telDB.Close()

	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	issues, err := s.Tel.ListIssues(ctx, teldb.ListIssuesParams{AppID: 1, WithResolved: true, Lim: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Count != 3 || issues[0].ResolvedAt != "" {
		t.Errorf("issues %+v, want one open issue of 3", issues)
	}
}
