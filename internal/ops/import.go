package ops

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// ImportDump adds a dump made elsewhere (another host, Temps, Coolify...)
// to the database's backups, sealed and uploaded like one hakobu made, so
// moving a database in is restoring that backup: Check tries it in a
// scratch database, Restore puts it in place. It takes pg_dump's custom
// format (pg_dump -Fc) only, for the reason in internal/backup.
func ImportDump(s *store.Store, dbName string, dump io.Reader) (id int64, err error) {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return 0, fmt.Errorf("database %q not found: %w", dbName, err)
	}
	r := bufio.NewReader(dump)
	if magic, _ := r.Peek(5); !bytes.Equal(magic, []byte("PGDMP")) {
		return 0, errors.New("not a pg_dump custom-format dump: make it with pg_dump -Fc")
	}
	target, err := projectBackupTarget(s, d.ProjectID)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(tmpDir, "import-*.enc")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	fileKey, err := secret.NewFileKey()
	if err != nil {
		return 0, err
	}
	sum := sha256.New()
	w, err := secret.NewFileWriterWithKey(io.MultiWriter(f, sum), fileKey)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(w, r); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%s/%s-import.dump.enc", d.Name, time.Now().UTC().Format("20060102-150405"))
	parts, err := uploadParts(s, target, key, f, info.Size())
	if err != nil {
		return 0, err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{
		Database: d.Name, ObjectKey: key, Parts: int64(parts), SizeBytes: info.Size(), SHA256: hex.EncodeToString(sum.Sum(nil)), FileKey: secret.String(fileKey),
		AccountID: target.AccountID, Bucket: target.Bucket,
	})
}
