package ops

import (
	"fmt"
	"time"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/store"
)

func SetBackupStorage(s *store.Store, dbName, storageName string) error {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return err
	}
	if storageName != "" {
		st, err := s.GetStorage(ctx(), storageName)
		if err != nil || st.ProjectID != d.ProjectID {
			return fmt.Errorf("storage %q not found in this project", storageName)
		}
	}
	return s.SetDatabaseBackupStorage(ctx(), store.SetDatabaseBackupStorageParams{Name: dbName, BackupStorage: storageName})
}

func backupClient(s *store.Store, d store.Database) (*backup.Client, error) {
	if d.BackupStorage == "" {
		return nil, fmt.Errorf("pick a backup storage for %s first", d.Name)
	}
	st, err := s.GetStorage(ctx(), d.BackupStorage)
	if err != nil {
		return nil, err
	}
	return hostClient(st)
}

// BackupDatabase uploads a gzipped pg_dump to the database's backup storage.
func BackupDatabase(s *store.Store, dbName string) error {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return err
	}
	client, err := backupClient(s, d)
	if err != nil {
		return err
	}
	dump, err := backup.DumpDatabase(ctx(), PostgresContainer, d.User, d.Name)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("backups/%s/%s.sql.gz", d.Name, time.Now().UTC().Format("20060102-150405"))
	if err := client.PutObject(key, dump, "application/gzip"); err != nil {
		return err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{Database: d.Name, ObjectKey: key, SizeBytes: int64(len(dump))})
}

// RestoreDatabase replays a backup into the database. The dump is plain SQL,
// so rows that already exist cause errors rather than being overwritten.
func RestoreDatabase(s *store.Store, dbName, objectKey string) error {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return err
	}
	client, err := backupClient(s, d)
	if err != nil {
		return err
	}
	dump, err := client.GetObject(objectKey)
	if err != nil {
		return err
	}
	return backup.RestoreDatabase(ctx(), PostgresContainer, d.User, d.Name, dump)
}

// BackupAll backs up every database that has a backup storage.
func BackupAll(s *store.Store) map[string]error {
	results := map[string]error{}
	dbs, err := s.ListDatabases(ctx())
	if err != nil {
		return results
	}
	for _, d := range dbs {
		if d.BackupStorage != "" {
			results[d.Name] = BackupDatabase(s, d.Name)
		}
	}
	return results
}
