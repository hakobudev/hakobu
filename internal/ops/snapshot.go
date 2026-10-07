package ops

import (
	"database/sql"
	"fmt"
	"io"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/store"
)

// Snapshots of an app's database for Rollback with data live on the app's
// node (see node.Local.SaveDump); the panel records which database the
// snapshot is of and when it was taken.

// takeSnapshot dumps the app's database before a deploy; "" if the app has
// none or the dump failed (the deploy goes on without one).
func takeSnapshot(s *store.Store, app store.App, out io.Writer) node.Dump {
	if !app.LinkedDbID.Valid {
		return ""
	}
	d, err := s.GetDatabaseByID(ctx(), app.LinkedDbID.Int64)
	if err == nil {
		fmt.Fprintln(out, "saving a snapshot of database", d.Name, "for rollback")
		var dump node.Dump
		if dump, err = AppNode(s, app).SaveDump(ctx(), dbSpec(d)); err == nil {
			return dump
		}
	}
	fmt.Fprintln(out, "warning: no database snapshot, Rollback will only restore the code:", err)
	return ""
}

// keepSnapshot makes a dump the app's snapshot of db, or with dump "" drops
// the snapshot: it must always belong to the Previous build.
func keepSnapshot(s *store.Store, app store.App, db sql.NullInt64, dump node.Dump) error {
	if err := AppNode(s, app).KeepSnapshot(app.Key(), dump); err != nil {
		return err
	}
	if dump == "" {
		return s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{ID: app.ID})
	}
	return s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{ID: app.ID, SnapshotDbID: db, SnapshotAt: time.Now().UTC().Format("2006-01-02T15:04:05Z")})
}

// DataRollbackBlocker says why Rollback can't return the app's data, ""
// if it can.
func DataRollbackBlocker(s *store.Store, app store.App) string {
	switch {
	case !app.SnapshotDbID.Valid:
		return "there's no database snapshot from before the last deploy"
	case app.SnapshotDbID != app.LinkedDbID:
		return "the snapshot is of a database that isn't the app's any more"
	}
	if !AppNode(s, app).HasSnapshot(app.Key()) {
		return "the snapshot file is missing"
	}
	if users, err := s.AppsUsingDatabase(ctx(), app.LinkedDbID); err != nil || len(users) != 1 {
		return "other apps use its database too, and would lose their data as well"
	}
	return ""
}
