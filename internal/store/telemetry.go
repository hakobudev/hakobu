package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hakobudev/hakobu/internal/ingest"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

// The apps' telemetry lives in telemetry.db next to the panel's database:
// it's the bulk of the data, changes all the time and is worth little a
// month later, so it isn't in the backups or the copy kept for rolling
// hakobu back. Losing it loses only history; hakobu starts a new one
// whenever it can't use the file.
const telemetryFile = "telemetry.db"

// eventRetention is how long errors, crashes, outages and usage are
// kept; the apps' logs go after PruneOldData's retentionDays.
const eventRetention = 30 * 24 * time.Hour

// traceRetention is how long traces are kept.
const traceRetention = 7 * 24 * time.Hour

// telSecretColumns are the columns of telemetry.db sqlc maps to
// secret.String (sqlc.yaml): apps' errors and logs may hold their users'
// data.
var telSecretColumns = map[string][]string{
	"telemetry_events": {"message", "payload"},
	// A span's description holds SQL and URLs, with their values.
	"traces": {"slow_span", "payload"},
	// The panel's errors quote what failed: hosts, names, the odd URL.
	"panel_log": {"message"},
}

// openTelemetry opens telemetry.db in dir, starting a new one when it was
// written by a newer hakobu or under a master key that's gone.
func openTelemetry(dir string, keyMissing bool) (*sql.DB, error) {
	path := filepath.Join(dir, telemetryFile)
	if keyMissing {
		removeTelemetry(path)
	}
	open := func() (*sql.DB, error) {
		// auto_vacuum only takes effect on a new file: pruning then gives
		// the space back without rewriting the whole file.
		db, err := sql.Open("sqlite", path+"?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		if err := migrateWith(db, telemetryMigrationFiles, "telemetry/migrations/*.sql"); err != nil {
			db.Close()
			return nil, err
		}
		if err := ensureIndexes(db, telemetryIndexes); err != nil {
			db.Close()
			return nil, err
		}
		return db, nil
	}
	db, err := open()
	if errors.Is(err, errNewerSchema) {
		panellog.Warn(telemetryFile, "is from a newer hakobu; starting a new one")
		removeTelemetry(path)
		db, err = open()
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func removeTelemetry(path string) {
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			panellog.Error("failed to remove", f+":", err)
		}
	}
}

// reencryptTelemetry encrypts the telemetry with the current master key.
func (s *Store) reencryptTelemetry() error {
	tx, err := s.telDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reencrypt(tx, telSecretColumns); err != nil {
		return err
	}
	return tx.Commit()
}

// dropTelemetry empties the tables holding encrypted telemetry.
func (s *Store) dropTelemetry() {
	for table := range telSecretColumns {
		if _, err := s.telDB.Exec(`DELETE FROM ` + table); err != nil {
			panellog.Error("failed to empty", table+":", err)
		}
	}
}

// deleteTelemetryOf removes the telemetry of a deleted app.
func (s *Store) deleteTelemetryOf(ctx context.Context, app int64) error {
	for _, target := range []string{fmt.Sprintf("app:%d", app), fmt.Sprintf("worker:%d", app)} {
		if err := s.Tel.DeleteSamplesOf(ctx, target); err != nil {
			return err
		}
	}
	for _, del := range []func(context.Context, int64) error{s.Tel.DeleteTracesOfApp, s.Tel.DeleteTraceRoutesOfApp, s.Tel.DeleteTelemetryOfApp, s.Tel.DeleteResolvedIssuesOfApp} {
		if err := del(ctx, app); err != nil {
			return err
		}
	}
	return nil
}

// pruneTelemetry deletes logs older than retentionDays and other telemetry
// older than eventRetention, and gives the space back.
func (s *Store) pruneTelemetry(ctx context.Context, retentionDays int) error {
	return pruneTel(ctx, s.Tel, s.telDB, retentionDays)
}

func pruneTel(ctx context.Context, q *teldb.Queries, db *sql.DB, retentionDays int) error {
	if err := q.PruneLogs(ctx, timestamp(time.Now().AddDate(0, 0, -retentionDays))); err != nil {
		return err
	}
	if err := q.PruneEvents(ctx, timestamp(time.Now().Add(-eventRetention))); err != nil {
		return err
	}
	// Resolved before the oldest event kept: whatever it covered is gone.
	if err := q.PruneResolvedIssues(ctx, timestamp(time.Now().Add(-eventRetention))); err != nil {
		return err
	}
	if err := q.PrunePanelLog(ctx, timestamp(time.Now().Add(-eventRetention))); err != nil {
		return err
	}
	if err := q.PruneDataChecks(ctx, time.Now().Add(-eventRetention).Unix()); err != nil {
		return err
	}
	traceCutoff := time.Now().Add(-traceRetention).Unix()
	if err := q.PruneTraces(ctx, traceCutoff); err != nil {
		return err
	}
	if err := q.PruneTraceRoutes(ctx, traceCutoff); err != nil {
		return err
	}
	// The rings overwrite their own slots; this drops targets gone for good.
	if err := q.PruneSamples(ctx, time.Now().Add(-eventRetention).Unix()); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
	return err
}

// fingerprintOldEvents gives the events kept from before issues the
// fingerprint they'd get now, so they group with what comes in after
// instead of standing alone until they're pruned. It needs the master key
// to read them, and does nothing once they're done: no new event is
// marked 'event:<id>'. One resolved alone is open again, in its group.
func fingerprintOldEvents(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := teldb.New(tx)
	for after := int64(0); ; {
		rows, err := q.ListUnfingerprinted(ctx, teldb.ListUnfingerprintedParams{ID: after, Limit: 500})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			fp := ingest.Fingerprint(r.Kind, r.Level, []byte(r.Payload))
			if err := q.SetFingerprint(ctx, teldb.SetFingerprintParams{Fingerprint: fp, ID: r.ID}); err != nil {
				return err
			}
			after = r.ID
		}
	}
	if err := q.DeleteUnfingerprintedResolved(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// NodeTelemetry is the telemetry.db of a server other than the panel's:
// its apps' errors, logs and traces, sealed with its own master key, which
// the panel reads over the link and never keeps.
type NodeTelemetry struct {
	Q  *teldb.Queries
	db *sql.DB
}

// OpenNodeTelemetry opens the server's telemetry.db in dir, with its
// master key loaded.
func OpenNodeTelemetry(dir string) (*NodeTelemetry, error) {
	db, err := openTelemetry(dir, false)
	if err != nil {
		return nil, err
	}
	if err := fingerprintOldEvents(context.Background(), db); err != nil {
		panellog.Warn("grouping the errors kept from before issues:", err)
	}
	return &NodeTelemetry{Q: teldb.New(db), db: db}, nil
}

// Prune is PruneOldData's for a server's telemetry.
func (t *NodeTelemetry) Prune(ctx context.Context, retentionDays int) error {
	return pruneTel(ctx, t.Q, t.db, retentionDays)
}
