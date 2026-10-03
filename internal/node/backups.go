package node

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/x0ryz/hakobu/internal/backup"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/secret"
)

// Backups of databases and volumes go from the node straight to R2. The
// panel gives the node, for each backup, the key to seal it with and
// presigned URLs for its parts, made with credentials that never leave
// the panel: a URL lets the node write or read that part alone, for an
// hour. The panel keeps the key, the SHA-256 and the parts' count.

// Upload is where a backup goes: sealed with FileKey (secret.NewFileKey),
// in parts of at most PartSize, part i to URL(i), a presigned PUT.
type Upload struct {
	FileKey  string
	PartSize int64
	URL      func(part int) (string, error)
}

// Uploaded is a backup as it went up.
type Uploaded struct {
	Parts  int
	Size   int64  // of the sealed file
	SHA256 string // of the sealed file, checked before it's restored
}

// Download is an uploaded backup: Parts parts, part i at URL(i), a
// presigned GET. FileKey "" is a dump from before backups were sealed,
// read as it is.
type Download struct {
	FileKey string
	SHA256  string
	Parts   int
	URL     func(part int) (string, error)
}

// tmpDir holds backups on their way; it's on the data disk rather than
// /tmp, which may be a small RAM disk.
const tmpDir = "data/tmp"

// httpClient sends the parts; a part takes as long as it takes.
var httpClient = &http.Client{}

// BackupDatabase uploads a sealed pg_dump of the database.
func (Local) BackupDatabase(ctx context.Context, d DBSpec, up Upload) (Uploaded, error) {
	return uploadSealed(ctx, up, func(w io.Writer) error {
		return backup.DumpDatabase(ctx, PostgresContainer, d.User, d.Name, w)
	})
}

// BackupVolume uploads a sealed tar of the volume, archived with the
// containers that mount it paused (ArchiveVolume).
func (n Local) BackupVolume(ctx context.Context, app, name string, up Upload) (Uploaded, error) {
	return uploadSealed(ctx, up, func(w io.Writer) error {
		return n.archiveVolume(ctx, app, name, w)
	})
}

// RestoreDatabase replays a backup into the database, once all of it is
// downloaded and matches its SHA-256. Objects and rows that already exist
// cause errors rather than being overwritten.
func (Local) RestoreDatabase(ctx context.Context, d DBSpec, dl Download) error {
	body, err := download(ctx, dl)
	if err != nil {
		return err
	}
	defer body.Close()
	return backup.RestoreDatabase(ctx, PostgresContainer, d.User, d.Name, body)
}

// VerifyDatabaseBackup restores a backup into a scratch database next to
// the real one, counts its tables and drops it.
func (Local) VerifyDatabaseBackup(ctx context.Context, d DBSpec, dl Download) (tables int, err error) {
	body, err := download(ctx, dl)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	// Database names can't contain dots, so this never clashes with one.
	scratch := "hakobu.verify." + d.Name
	drop, err := scratchDatabase(ctx, d, scratch)
	if err != nil {
		return 0, err
	}
	defer drop()
	if err := backup.RestoreDatabase(ctx, PostgresContainer, d.User, scratch, body); err != nil {
		return 0, err
	}
	return backup.CountTables(ctx, PostgresContainer, d.User, scratch)
}

// RestoreVolume replaces the volume's contents with a backup. All of it is
// downloaded and checked before anything stops; then the app and its
// worker are stopped, and the app is started again after (the panel
// starts the worker, whose variables it holds).
func (n Local) RestoreVolume(ctx context.Context, app AppSpec, name string, dl Download, out io.Writer) error {
	fmt.Fprintln(out, "downloading the backup")
	body, err := download(ctx, dl)
	if err != nil {
		return err
	}
	defer body.Close()
	fmt.Fprintln(out, "stopping", app.Name)
	if err := n.StopApp(ctx, app); err != nil {
		return err
	}
	defer n.StartApp(ctx, app, out)
	fmt.Fprintln(out, "replacing the contents of volume", name)
	if err := backup.RestoreVolume(ctx, config.PostgresImage, Volume(app.Name, name), body); err != nil {
		return err
	}
	fmt.Fprintln(out, "restored; starting", app.Name, "again")
	return nil
}

// VerifyVolumeBackup downloads a backup and reads the whole tar in it,
// counting its files.
func (Local) VerifyVolumeBackup(ctx context.Context, dl Download) (files int, err error) {
	body, err := download(ctx, dl)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	tr := tar.NewReader(body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("the tar is broken: %w", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil { // reads (and authenticates) every byte
			return files, err
		}
		if h.Typeflag != tar.TypeDir {
			files++
		}
	}
}

// uploadSealed seals what write produces into a temporary file (an upload
// needs its size up front) and uploads it in parts. A backup in the bucket
// is no use to whoever gets at the bucket without its key.
func uploadSealed(ctx context.Context, up Upload, write func(io.Writer) error) (Uploaded, error) {
	if up.FileKey == "" || up.PartSize <= 0 {
		return Uploaded{}, errors.New("a backup needs a key and a part size")
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return Uploaded{}, err
	}
	f, err := os.CreateTemp(tmpDir, "backup-*.enc")
	if err != nil {
		return Uploaded{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha256.New()
	w, err := secret.NewFileWriterWithKey(io.MultiWriter(f, sum), up.FileKey)
	if err != nil {
		return Uploaded{}, err
	}
	if err := write(w); err != nil {
		return Uploaded{}, err
	}
	if err := w.Close(); err != nil {
		return Uploaded{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return Uploaded{}, err
	}
	u := Uploaded{Size: info.Size(), SHA256: hex.EncodeToString(sum.Sum(nil))}
	for offset := int64(0); offset < u.Size || u.Parts == 0; offset += up.PartSize {
		n := min(up.PartSize, u.Size-offset)
		if err := putPart(ctx, up, u.Parts, io.NewSectionReader(f, offset, n), n); err != nil {
			return Uploaded{}, err
		}
		u.Parts++
	}
	return u, nil
}

func putPart(ctx context.Context, up Upload, part int, body io.Reader, size int64) error {
	u, err := up.URL(part)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading part %d: %w", part, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("uploading part %d failed (%d): %s", part, resp.StatusCode, msg)
	}
	return nil
}

// tempReader is a temporary file read through r (the backup opened),
// removed on Close.
type tempReader struct {
	io.Reader
	f *os.File
}

func (t tempReader) Close() error {
	err := t.f.Close()
	os.Remove(t.f.Name())
	return err
}

// download fetches a backup into a temporary file and checks it against
// its SHA-256, so a backup changed in the bucket is never restored, then
// opens it with its key. Closing it removes the file.
func download(ctx context.Context, dl Download) (io.ReadCloser, error) {
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, "restore-*")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	sum := sha256.New()
	for part := range dl.Parts {
		if err := getPart(ctx, dl, part, io.MultiWriter(f, sum)); err != nil {
			return fail(err)
		}
	}
	if hex.EncodeToString(sum.Sum(nil)) != dl.SHA256 {
		return fail(errors.New("the backup in the bucket doesn't match the one hakobu made (SHA-256 differs), not restoring it"))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	var r io.Reader = f
	if dl.FileKey != "" {
		if r, err = secret.NewFileReaderKey(f, dl.FileKey); err != nil {
			return fail(err)
		}
	}
	return tempReader{r, f}, nil
}

func getPart(ctx context.Context, dl Download, part int, w io.Writer) error {
	u, err := dl.URL(part)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading part %d: %w", part, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("downloading part %d failed (%d): %s", part, resp.StatusCode, msg)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// archiveVolume writes a tar of the volume to w, with the running
// containers that mount it paused meanwhile. One that can't be paused is
// copied running: a backup that may need the app's own recovery beats none.
func (Local) archiveVolume(ctx context.Context, app, name string, w io.Writer) error {
	volume := Volume(app, name)
	users, err := deploy.RunningWithVolume(ctx, volume)
	if err != nil {
		fmt.Println("backup of", volume+": copying without pausing its containers:", err)
	}
	for _, c := range users {
		if err := deploy.PauseContainer(ctx, c); err != nil {
			fmt.Println("backup of", volume+": copying while", c, "runs:", err)
			continue
		}
		defer func() {
			if err := deploy.UnpauseContainer(ctx, c); err != nil {
				fmt.Println("failed to unpause", c+":", err)
			}
		}()
	}
	return backup.ArchiveVolume(ctx, config.PostgresImage, volume, w)
}
