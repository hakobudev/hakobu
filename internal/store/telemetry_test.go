package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store/teldb"
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
		if _, err := db.Exec(`INSERT INTO telemetry_events (app_name, kind) VALUES ('web', 'error')`); err != nil {
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
	if err := s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppName: "web", Kind: "error", Message: "boom"}); err != nil {
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
