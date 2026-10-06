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

	"github.com/x0ryz/hakobu/internal/panellog"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// ImportDump adds a dump made elsewhere (another host, Temps, Coolify...)
// to the database's backups, sealed and uploaded like one hakobu made, so
// moving a database in is restoring that backup: Check tries it in a
// scratch database, Restore puts it in place. It takes pg_dump's custom
// format (pg_dump -Fc) only, for the reason in internal/backup.
//
// The panel can't seal it with the key of the database's server, which it
// never has: it uploads it sealed with a key of its own, and the server
// opens that and seals it again as a backup of its own (Reseal).
func ImportDump(s *store.Store, dbID int64, dump io.Reader) (id int64, err error) {
	d, err := s.GetDatabaseByID(ctx(), dbID)
	if err != nil {
		return 0, fmt.Errorf("database not found: %w", err)
	}
	r := bufio.NewReader(dump)
	if magic, _ := r.Peek(5); !bytes.Equal(magic, []byte("PGDMP")) {
		return 0, errors.New("not a pg_dump custom-format dump: make it with pg_dump -Fc")
	}
	target, err := projectBackupTarget(s, d.ProjectID)
	if err != nil {
		return 0, err
	}
	n, err := databaseNode(s, d)
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
	stamp := time.Now().UTC().Format("20060102-150405")
	sent := fmt.Sprintf("uploads/%s/%s-import.enc", d.Key(), stamp)
	parts, err := uploadParts(s, target, sent, f, info.Size())
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := deleteParts(s, target, sent, int64(parts)); err != nil {
			panellog.Warn("the uploaded dump of", d.Name, "stays in the bucket at", sent+":", err)
		}
	}()
	dl, err := backupDownload(s, target, sent, int64(parts), hex.EncodeToString(sum.Sum(nil)), "")
	if err != nil {
		return 0, err
	}
	dl.FileKey = fileKey
	key := fmt.Sprintf("%s/%s-import.dump.enc", d.Key(), stamp)
	up, err := newUpload(s, target, key)
	if err != nil {
		return 0, err
	}
	obj, err := n.Reseal(ctx(), dl, up)
	if err != nil {
		return 0, err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{
		DatabaseID: d.ID, ObjectKey: key, Parts: int64(obj.Parts), SizeBytes: obj.Size, SHA256: obj.SHA256, KeyFingerprint: obj.KeyFingerprint,
		AccountID: target.AccountID, Bucket: target.Bucket,
	})
}
