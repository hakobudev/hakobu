// Package backups keeps a server's databases and volumes in S3 buckets,
// sealed with age to the server and its owner, so either can restore them
// and nobody with the bucket alone can read them. A backup goes to the
// primary bucket and to copies (another provider, with a key that can only
// write), is read back from the primary and restored into scratch before
// it counts, and is listed from the primary bucket itself: there's no
// catalog to lose.
package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/s3"
)

// Store is a bucket, under a prefix of the server's.
type Store interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// Target is a bucket backups go to.
type Target struct {
	Name   string // for messages: "r2", "hetzner"
	Store  Store
	Prefix string // "hakobu/nova-1/"
}

// Source makes and loads dumps and volume archives (node.Local).
type Source interface {
	DumpDatabase(ctx context.Context, d node.DBSpec, w io.Writer) error
	CheckDump(ctx context.Context, d node.DBSpec, dump io.Reader) (tables int, err error)
	LoadDump(ctx context.Context, d node.DBSpec, dump io.Reader) error
	ArchiveVolume(ctx context.Context, app, name string, w io.Writer) error
	CheckArchive(archive io.Reader) (files int, err error)
	LoadArchive(ctx context.Context, app node.AppSpec, name string, archive io.Reader, out io.Writer) error
}

type Kind string

const (
	Database Kind = "database"
	Volume   Kind = "volume"
)

// Manifest describes a backup that was uploaded and checked; one without
// a manifest never finished.
type Manifest struct {
	Kind    Kind      `json:"kind"`
	Project string    `json:"project"`
	Name    string    `json:"name"` // the database, or "app/volume"
	At      time.Time `json:"at"`
	Size    int64     `json:"size"`   // sealed
	SHA256  string    `json:"sha256"` // of the sealed file
	Parts   int       `json:"parts"`
	// Recipients could open it when it was made.
	Recipients []string `json:"recipients"`
	// Checked is what reading it back found: "12 tables", "340 files".
	Checked string `json:"checked"`
}

const stamp = "20060102T150405Z"

// dir is where the backup's parts and manifest are.
func (m Manifest) dir() string {
	return fmt.Sprintf("%s/%ss/%s/%s/", m.Project, m.Kind, m.Name, m.At.UTC().Format(stamp))
}

func (m Manifest) String() string {
	return fmt.Sprintf("%s %s/%s of %s (%s, %s)", m.Kind, m.Project, m.Name, m.At.UTC().Format("2006-01-02 15:04"), size(m.Size), m.Checked)
}

// Vault makes, lists, restores and rotates a server's backups.
type Vault struct {
	Source  Source
	Primary Target
	// Copies get every backup too; they're written, never read or
	// pruned (their buckets' own lifecycle rules expire old ones).
	Copies     []Target
	Recipients []string // age public keys: the server's and the owner's
	Identity   age.Identity
	PartSize   int64  // default 64 MB
	TempDir    string // default os.TempDir()
	Now        func() time.Time
}

// BackupDatabase backs up a project's database.
func (v *Vault) BackupDatabase(ctx context.Context, project, name string, d node.DBSpec) (Manifest, error) {
	m := Manifest{Kind: Database, Project: project, Name: name}
	return v.backup(ctx, m,
		func(w io.Writer) error { return v.Source.DumpDatabase(ctx, d, w) },
		func(r io.Reader) (string, error) {
			n, err := v.Source.CheckDump(ctx, d, r)
			return fmt.Sprintf("%d tables", n), err
		})
}

// BackupVolume backs up an app's volume; container is the app's name on
// the server.
func (v *Vault) BackupVolume(ctx context.Context, project, app, container, volume string) (Manifest, error) {
	m := Manifest{Kind: Volume, Project: project, Name: app + "/" + volume}
	return v.backup(ctx, m,
		func(w io.Writer) error { return v.Source.ArchiveVolume(ctx, container, volume, w) },
		func(r io.Reader) (string, error) {
			n, err := v.Source.CheckArchive(r)
			return fmt.Sprintf("%d files", n), err
		})
}

// backup seals what write writes into a temporary file, uploads it in
// parts to every target, reads it back from the primary through check,
// and only then writes its manifest. A copy that fails is an error after
// the backup is safe in the primary.
func (v *Vault) backup(ctx context.Context, m Manifest, write func(io.Writer) error, check func(io.Reader) (string, error)) (Manifest, error) {
	m.At, m.Recipients = v.now(), v.Recipients
	recipients, err := v.recipients()
	if err != nil {
		return m, err
	}
	f, err := os.CreateTemp(v.TempDir, "backup-*.age")
	if err != nil {
		return m, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum := sha256.New()
	w, err := age.Encrypt(io.MultiWriter(f, sum), recipients...)
	if err != nil {
		return m, err
	}
	if err := write(w); err != nil {
		return m, fmt.Errorf("%s %s: %w", m.Kind, m.Name, err)
	}
	if err := w.Close(); err != nil {
		return m, err
	}
	st, err := f.Stat()
	if err != nil {
		return m, err
	}
	m.Size, m.SHA256 = st.Size(), hex.EncodeToString(sum.Sum(nil))

	if m.Parts, err = v.upload(ctx, v.Primary, m, f); err != nil {
		return m, err
	}
	body, err := v.Open(ctx, m)
	if err != nil {
		return m, fmt.Errorf("reading the backup back: %w", err)
	}
	m.Checked, err = check(body)
	body.Close()
	if err != nil {
		return m, fmt.Errorf("the backup didn't restore: %w", err)
	}
	manifest, _ := json.MarshalIndent(m, "", "  ")
	if err := v.Primary.Store.Put(ctx, v.Primary.Prefix+m.dir()+"manifest.json", manifest); err != nil {
		return m, err
	}
	var errs []error
	for _, t := range v.Copies {
		_, err := v.upload(ctx, t, m, f)
		if err == nil {
			err = t.Store.Put(ctx, t.Prefix+m.dir()+"manifest.json", manifest)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("copy to %s: %w", t.Name, err))
		}
	}
	return m, errors.Join(errs...)
}

func (v *Vault) upload(ctx context.Context, t Target, m Manifest, f *os.File) (parts int, err error) {
	partSize := v.PartSize
	if partSize <= 0 {
		partSize = 64 << 20
	}
	buf := make([]byte, partSize)
	for offset := int64(0); offset < m.Size || parts == 0; offset += partSize {
		n, err := f.ReadAt(buf[:min(partSize, m.Size-offset)], offset)
		if err != nil && err != io.EOF {
			return parts, err
		}
		if err := t.Store.Put(ctx, fmt.Sprintf("%s%spart-%04d", t.Prefix, m.dir(), parts), buf[:n]); err != nil {
			return parts, fmt.Errorf("%s: part %d: %w", t.Name, parts, err)
		}
		parts++
	}
	return parts, nil
}

// List is every finished backup of the project in the primary bucket,
// newest first.
func (v *Vault) List(ctx context.Context, project string) ([]Manifest, error) {
	keys, err := v.Primary.Store.List(ctx, v.Primary.Prefix+project+"/")
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, k := range keys {
		if !strings.HasSuffix(k, "/manifest.json") {
			continue
		}
		body, err := v.Primary.Store.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		var m Manifest
		err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&m)
		body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int { return b.At.Compare(a.At) })
	return out, nil
}

// Open downloads a backup whole into a temporary file, checks it against
// its SHA-256 and opens it with the vault's identity. Closing it removes
// the file.
func (v *Vault) Open(ctx context.Context, m Manifest) (io.ReadCloser, error) {
	f, err := os.CreateTemp(v.TempDir, "restore-*.age")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (io.ReadCloser, error) {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	sum := sha256.New()
	for part := range m.Parts {
		body, err := v.Primary.Store.Get(ctx, fmt.Sprintf("%s%spart-%04d", v.Primary.Prefix, m.dir(), part))
		if err != nil {
			return fail(err)
		}
		_, err = io.Copy(io.MultiWriter(f, sum), body)
		body.Close()
		if err != nil {
			return fail(err)
		}
	}
	if hex.EncodeToString(sum.Sum(nil)) != m.SHA256 {
		return fail(errors.New("the backup in the bucket isn't the one hakobu made (its SHA-256 differs); not using it"))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	r, err := age.Decrypt(f, v.Identity)
	var none *age.NoIdentityMatchError
	if errors.As(err, &none) {
		return fail(fmt.Errorf("it's sealed to %s, and this key isn't among them", strings.Join(m.Recipients, ", ")))
	}
	if err != nil {
		return fail(err)
	}
	return &tempFile{r, f}, nil
}

type tempFile struct {
	io.Reader
	f *os.File
}

func (t *tempFile) Close() error {
	err := t.f.Close()
	os.Remove(t.f.Name())
	return err
}

// RestoreDatabase replaces the database's content with the backup, once
// all of it is downloaded and checked.
func (v *Vault) RestoreDatabase(ctx context.Context, m Manifest, d node.DBSpec) error {
	if m.Kind != Database {
		return fmt.Errorf("%s isn't a database backup", m)
	}
	body, err := v.Open(ctx, m)
	if err != nil {
		return err
	}
	defer body.Close()
	return v.Source.LoadDump(ctx, d, body)
}

// RestoreVolume replaces the volume's contents with the backup, the app
// stopped meanwhile.
func (v *Vault) RestoreVolume(ctx context.Context, m Manifest, app node.AppSpec, volume string, out io.Writer) error {
	if m.Kind != Volume {
		return fmt.Errorf("%s isn't a volume backup", m)
	}
	body, err := v.Open(ctx, m)
	if err != nil {
		return err
	}
	defer body.Close()
	return v.Source.LoadArchive(ctx, app, volume, body, out)
}

// lockedTooLong is how old a backup the bucket's lock still keeps may be
// before that's an error: a rule longer than the rotation's makes the
// bucket keep what hakobu no longer counts.
const lockedTooLong = 31 * 24 * time.Hour

// Rotate deletes a project's backups past what's kept of each database and
// volume: the newest daily ones, and the newest of each of the weeks
// before. The bucket's lock may refuse to delete one yet (one made by hand
// the day of another, under a 7-day lock); it's tried again next time, and
// is an error only once it's older than lockedTooLong.
func (v *Vault) Rotate(ctx context.Context, project string, daily, weekly int) (deleted []Manifest, err error) {
	all, err := v.List(ctx, project)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, m := range Expired(all, daily, weekly) {
		if err := v.delete(ctx, m); err != nil {
			if !errors.Is(err, s3.ErrLocked) || v.now().Sub(m.At) > lockedTooLong {
				errs = append(errs, err)
			}
			continue
		}
		deleted = append(deleted, m)
	}
	if err := v.dropUnfinished(ctx, project); err != nil {
		errs = append(errs, err)
	}
	return deleted, errors.Join(errs...)
}

// dropUnfinished deletes the parts of backups that never got a manifest
// (the upload or its check failed) once they're a day old: a younger one
// may be uploading now.
func (v *Vault) dropUnfinished(ctx context.Context, project string) error {
	keys, err := v.Primary.Store.List(ctx, v.Primary.Prefix+project+"/")
	if err != nil {
		return err
	}
	parts, finished := map[string][]string{}, map[string]bool{}
	for _, k := range keys {
		dir, file := k[:strings.LastIndex(k, "/")+1], k[strings.LastIndex(k, "/")+1:]
		if file == "manifest.json" {
			finished[dir] = true
		} else {
			parts[dir] = append(parts[dir], k)
		}
	}
	var errs []error
	for dir, ks := range parts {
		at, err := time.Parse(stamp, path.Base(dir))
		if finished[dir] || err != nil || v.now().Sub(at) < 24*time.Hour {
			continue
		}
		for _, k := range ks {
			if err := v.Primary.Store.Delete(ctx, k); err != nil && (!errors.Is(err, s3.ErrLocked) || v.now().Sub(at) > lockedTooLong) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// delete removes the manifest first: a backup half deleted is one that
// never finished, not one that seems whole.
func (v *Vault) delete(ctx context.Context, m Manifest) error {
	t := v.Primary
	if err := t.Store.Delete(ctx, t.Prefix+m.dir()+"manifest.json"); err != nil {
		return err
	}
	for part := range m.Parts {
		if err := t.Store.Delete(ctx, fmt.Sprintf("%s%spart-%04d", t.Prefix, m.dir(), part)); err != nil {
			return err
		}
	}
	return nil
}

// Expired are the backups past what's kept of each database and volume:
// the newest of each of the last daily days that have one, then the newest
// of each of the next weekly ISO weeks those don't already cover. Backups
// made by hand the same day count as that day's, not as days of their own.
func Expired(all []Manifest, daily, weekly int) []Manifest {
	groups := map[string][]Manifest{}
	for _, m := range all {
		k := string(m.Kind) + " " + m.Name
		groups[k] = append(groups[k], m)
	}
	var out []Manifest
	for _, g := range groups {
		slices.SortFunc(g, func(a, b Manifest) int { return b.At.Compare(a.At) })
		days, weeks := map[string]bool{}, 0
		covered := map[string]bool{} // weeks with a backup kept
		for _, m := range g {
			day := m.At.UTC().Format("2006-01-02")
			y, w := m.At.UTC().ISOWeek()
			week := fmt.Sprint(y, "-", w)
			switch {
			case days[day]:
				out = append(out, m) // an older one of a day kept already
			case len(days) < daily:
				days[day], covered[week] = true, true
			case !covered[week] && weeks < weekly:
				covered[week] = true
				weeks++
			default:
				out = append(out, m)
			}
		}
	}
	slices.SortFunc(out, func(a, b Manifest) int { return a.At.Compare(b.At) })
	return out
}

func (v *Vault) recipients() ([]age.Recipient, error) {
	var rs []age.Recipient
	for _, r := range v.Recipients {
		rc, err := age.ParseX25519Recipient(r)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", r, err)
		}
		rs = append(rs, rc)
	}
	if len(rs) == 0 {
		return nil, errors.New("no recipients to seal backups to")
	}
	return rs, nil
}

func (v *Vault) now() time.Time {
	if v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}
