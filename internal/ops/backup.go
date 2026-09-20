package ops

import (
	"context"
	"fmt"
	"time"

	"github.com/x0ryz/hako/internal/backup"
	"github.com/x0ryz/hako/internal/store"
)

// backupObjectKey namespaces every database's backups under its own
// prefix, timestamped so nothing ever collides and listing naturally comes
// back newest-last.
func backupObjectKey(dbName string, at time.Time) string {
	return fmt.Sprintf("backups/%s/%s.sql.gz", dbName, at.UTC().Format("20060102-150405"))
}

// SetDatabaseBackupStorage picks which storage dbName's backups upload to
// — chosen per database, right where its backups live, since different
// databases often want different buckets.
func SetDatabaseBackupStorage(s *store.Store, dbName, storageName string) error {
	if _, err := s.GetDatabase(dbName); err != nil {
		return fmt.Errorf("database %q not found: %w", dbName, err)
	}
	if _, err := s.GetStorage(storageName); err != nil {
		return fmt.Errorf("storage %q not found: %w", storageName, err)
	}
	return s.SetDatabaseBackupStorage(dbName, storageName)
}

// BackupDatabase dumps dbName via pg_dump and uploads the gzip-compressed
// result to whatever storage was picked for it (see
// SetDatabaseBackupStorage), recording it in the local backups table so
// the dashboard can list history without an API round trip.
func BackupDatabase(s *store.Store, dbName string) (objectKey string, sizeBytes int, err error) {
	db, err := s.GetDatabase(dbName)
	if err != nil {
		return "", 0, fmt.Errorf("database %q not found: %w", dbName, err)
	}
	if db.BackupStorage == "" {
		return "", 0, fmt.Errorf("no backup storage picked for %q yet — pick one on its Backups panel first", dbName)
	}

	ctx := context.Background()
	st, err := s.GetStorage(db.BackupStorage)
	if err != nil {
		return "", 0, fmt.Errorf("backup storage %q not found: %w", db.BackupStorage, err)
	}
	hostSt, err := hostReachableStorage(ctx, *st)
	if err != nil {
		return "", 0, err
	}

	dump, err := backup.DumpDatabase(ctx, db.ContainerName, db.DBUser, db.DBName)
	if err != nil {
		return "", 0, err
	}

	objectKey = backupObjectKey(dbName, time.Now())
	if err := backup.NewClient(hostSt).PutObject(objectKey, dump, "application/gzip"); err != nil {
		return "", 0, err
	}

	if err := s.CreateBackup(store.Backup{Database: dbName, ObjectKey: objectKey, SizeBytes: int64(len(dump))}); err != nil {
		return "", 0, err
	}

	return objectKey, len(dump), nil
}

// RestoreDatabase downloads objectKey (from dbName's own backup storage)
// and restores it into dbName. See backup.RestoreDatabase for what
// "restore" actually does to existing data.
func RestoreDatabase(s *store.Store, dbName, objectKey string) error {
	db, err := s.GetDatabase(dbName)
	if err != nil {
		return fmt.Errorf("database %q not found: %w", dbName, err)
	}
	if db.BackupStorage == "" {
		return fmt.Errorf("no backup storage picked for %q yet — pick one on its Backups panel first", dbName)
	}

	ctx := context.Background()
	st, err := s.GetStorage(db.BackupStorage)
	if err != nil {
		return fmt.Errorf("backup storage %q not found: %w", db.BackupStorage, err)
	}
	hostSt, err := hostReachableStorage(ctx, *st)
	if err != nil {
		return err
	}

	dump, err := backup.NewClient(hostSt).GetObject(objectKey)
	if err != nil {
		return err
	}

	return backup.RestoreDatabase(ctx, db.ContainerName, db.DBUser, db.DBName, dump)
}

func ListBackups(s *store.Store, dbName string) ([]store.Backup, error) {
	return s.ListBackups(dbName, 30)
}

// BackupAllDatabases runs BackupDatabase for every database that has a
// backup storage picked — the daily scheduled job (see cmd/agent.go's
// runBackupScheduler) calls this. A per-database failure is reported but
// doesn't stop the others from being attempted; a database with no
// storage picked yet is silently skipped, not an error.
func BackupAllDatabases(s *store.Store) (results map[string]error) {
	results = map[string]error{}
	databases, err := s.ListDatabases()
	if err != nil {
		return results
	}
	for _, db := range databases {
		if db.BackupStorage == "" {
			continue
		}
		_, _, err := BackupDatabase(s, db.Name)
		results[db.Name] = err
	}
	return results
}
