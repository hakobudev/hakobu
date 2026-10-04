package ops

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/s3"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

// Backups go to one R2 bucket in the connected Cloudflare account, written
// through the REST API with hakobu's Cloudflare API token: no S3 keys exist for it,
// so nothing an app holds can reach the backups. The bucket's lock keeps
// every file for backupLockDays, even from hakobu itself. Every backup is
// sealed (secret.NewFileWriterKey), so the bucket alone reveals nothing.
const backupLockDays = 7

// backupPartSize is under cloudflare.MaxObjectSize; tests make it smaller.
var backupPartSize int64 = 256 << 20

// BackupBucket is the R2 bucket backups go to, "" until they're set up.
func BackupBucket(s *store.Store) string {
	cf, err := s.GetCloudflare(ctx())
	if err != nil {
		return ""
	}
	return cf.BackupBucket
}

// SetupBackups creates the locked backup bucket; from then on every
// database is backed up daily.
func SetupBackups(s *store.Store) error {
	if BackupBucket(s) != "" {
		return nil
	}
	c, cf, err := cfClient(s)
	if err != nil {
		return err
	}
	bucket, err := createBackupBucket(c, cf.AccountID)
	if err != nil {
		return err
	}
	return s.SetBackupBucket(ctx(), bucket)
}

// createBackupBucket creates a locked bucket for backups in the account.
func createBackupBucket(c cloudflare.Client, account string) (string, error) {
	suffix, err := RandomHex(4)
	if err != nil {
		return "", err
	}
	bucket := "hakobu-backups-" + suffix
	if err := c.CreateBucket(account, bucket); err != nil {
		if cloudflare.R2Off(err) {
			return "", r2Explained(account, "this Cloudflare account", err)
		}
		return "", fmt.Errorf("creating the R2 bucket failed: %w", err)
	}
	if err := c.LockBucket(account, bucket, backupLockDays); err != nil {
		return "", err
	}
	return bucket, nil
}

// backupTarget is where a backup goes or went: a Cloudflare account and a
// bucket in it. The zero target is the panel's backup bucket, where every
// backup went before clients' accounts.
type backupTarget struct{ AccountID, Bucket string }

// r2 is r2At for the panel's backup bucket.
func r2(s *store.Store) (cloudflare.Client, string, string, error) {
	return r2At(s, backupTarget{})
}

// r2At returns an API client with a fresh token, the account and the
// bucket of t. It's called per request: a long upload can outlive a token.
func r2At(s *store.Store, t backupTarget) (cloudflare.Client, string, string, error) {
	a, err := accountByCloudflareID(s, t.AccountID)
	if err != nil {
		return cloudflare.Client{}, "", "", err
	}
	bucket := t.Bucket
	if bucket == "" && a.isPanel() {
		bucket = a.BackupBucket
	}
	if bucket == "" {
		return a.Client, "", "", errors.New("backups aren't set up yet (Settings → Backups)")
	}
	return a.Client, a.AccountID, bucket, nil
}

var clientBucketMu sync.Mutex

// projectBackupTarget is where the project's backups go: the panel's
// bucket, or a locked bucket in the client's account, made at its first
// backup. Backups are on for every project once they're set up.
func projectBackupTarget(s *store.Store, projectID int64) (backupTarget, error) {
	if BackupBucket(s) == "" {
		return backupTarget{}, errors.New("backups aren't set up yet (Settings → Backups)")
	}
	p, err := s.GetProjectByID(ctx(), projectID)
	if err != nil {
		return backupTarget{}, err
	}
	clientBucketMu.Lock()
	defer clientBucketMu.Unlock()
	a, err := projectAccount(s, p) // read under the lock: another backup may have made the bucket
	if err != nil {
		return backupTarget{}, err
	}
	if !a.isPanel() && a.BackupBucket == "" {
		if a.BackupBucket, err = createBackupBucket(a.Client, a.AccountID); err != nil {
			return backupTarget{}, fmt.Errorf("%s: %w", a.label(), err)
		}
		if err := s.SetCloudflareAccountBackupBucket(ctx(), store.SetCloudflareAccountBackupBucketParams{BackupBucket: a.BackupBucket, ID: a.ID}); err != nil {
			return backupTarget{}, err
		}
	}
	return backupTarget{a.AccountID, a.BackupBucket}, nil
}

// accountBackupBucket is the backup bucket of the account with
// Cloudflare's account ID id, "" if it has none.
func accountBackupBucket(s *store.Store, id string) string {
	a, err := accountByCloudflareID(s, id)
	if err != nil {
		return ""
	}
	return a.BackupBucket
}

func partKey(b string, i int) string { return fmt.Sprintf("%s/%03d", b, i) }

// partsReader reads a backup's parts one after another as one stream.
type partsReader struct {
	parts   int
	open    func(part int) (io.ReadCloser, error)
	next    int
	current io.ReadCloser
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.current == nil {
			if r.next == r.parts {
				return 0, io.EOF
			}
			var err error
			if r.current, err = r.open(r.next); err != nil {
				return 0, err
			}
			r.next++
		}
		n, err := r.current.Read(p)
		if err == io.EOF {
			r.current.Close()
			r.current, err = nil, nil
			if n == 0 {
				continue
			}
		}
		return n, err
	}
}

func (r *partsReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}

// Backup jobs: one backup, check or restore per database at a time; the
// panel shows the running one and how the last one went.

type DBJob struct {
	Running string // "backing up", ...; "" when idle
	Last    string // how the last job ended, "" if none since the agent started
	Failed  bool
}

var (
	dbJobsMu sync.Mutex
	dbJobs   = map[string]*DBJob{}
)

func dbJobsRunning() bool {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	for _, j := range dbJobs {
		if j.Running != "" {
			return true
		}
	}
	return false
}

func DatabaseJob(name string) DBJob {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	if j := dbJobs[name]; j != nil {
		return *j
	}
	return DBJob{}
}

func reserveDB(name, what string) error {
	if err := jobsClosed(); err != nil {
		return err
	}
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	j := dbJobs[name]
	if j == nil {
		j = &DBJob{}
		dbJobs[name] = j
	}
	if j.Running != "" {
		return fmt.Errorf("%s is busy %s, try again when it finishes", name, j.Running)
	}
	j.Running = what
	return nil
}

func finishDB(name, what string, err error) {
	dbJobsMu.Lock()
	defer dbJobsMu.Unlock()
	j := dbJobs[name]
	j.Running, j.Failed = "", err != nil
	j.Last = time.Now().Format("2006-01-02 15:04") + ": " + what + " "
	if err != nil {
		j.Last += "failed: " + err.Error()
	} else {
		j.Last += "done"
	}
}

func runDBJob(name, what string, fn func() error) error {
	if err := reserveDB(name, what); err != nil {
		return err
	}
	err := fn()
	finishDB(name, what, err)
	return err
}

// startDBJob runs the job in the background; it fails at once if the
// database is busy.
func startDBJob(name, what string, fn func() error) error {
	if err := reserveDB(name, what); err != nil {
		return err
	}
	go func() { finishDB(name, what, fn()) }()
	return nil
}

// StartBackup backs the database up in the background.
func StartBackup(s *store.Store, dbName string) error {
	if _, err := s.GetDatabase(ctx(), dbName); err != nil {
		return err
	}
	return startDBJob(dbName, "backing up", func() error { return backupAndCheck(s, dbName) })
}

// StartVerify restores a backup into a scratch database in the background.
func StartVerify(s *store.Store, dbName string, backupID int64) error {
	b, err := dbBackup(s, dbName, backupID)
	if err != nil {
		return err
	}
	return startDBJob(dbName, "checking a backup", func() error { return VerifyBackup(s, b.ID) })
}

// StartRestore replaces the database's content with a backup in the
// background.
func StartRestore(s *store.Store, dbName string, backupID int64) error {
	b, err := dbBackup(s, dbName, backupID)
	if err != nil {
		return err
	}
	return startDBJob(dbName, "restoring the backup of "+b.CreatedAt, func() error { return restoreBackup(s, b) })
}

// dbBackup looks up a backup and makes sure it belongs to dbName.
func dbBackup(s *store.Store, dbName string, id int64) (store.Backup, error) {
	b, err := s.GetBackup(ctx(), id)
	if err != nil || b.Database != dbName {
		return b, fmt.Errorf("no such backup of %s", dbName)
	}
	return b, nil
}

// backupAndCheck uploads a backup, restores it into a scratch database to
// prove it's usable, then drops old backups.
func backupAndCheck(s *store.Store, dbName string) error {
	id, err := BackupDatabase(s, dbName)
	if err != nil {
		return err
	}
	verifyErr := VerifyBackup(s, id)
	if err := RotateBackups(s, dbName, time.Now()); err != nil {
		fmt.Println("backup rotation of", dbName, "failed:", err)
	}
	if verifyErr != nil {
		return fmt.Errorf("the backup was uploaded, but restoring it failed: %w", verifyErr)
	}
	return nil
}

// BackupDatabase has the database's node upload a sealed pg_dump straight
// to the project's backup bucket.
func BackupDatabase(s *store.Store, dbName string) (id int64, err error) {
	d, err := s.GetDatabase(ctx(), dbName)
	if err != nil {
		return 0, err
	}
	target, err := projectBackupTarget(s, d.ProjectID)
	if err != nil {
		return 0, err
	}
	n, err := databaseNode(s, d)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%s/%s.dump.enc", d.Name, time.Now().UTC().Format("20060102-150405"))
	up, err := newUpload(s, target, key)
	if err != nil {
		return 0, err
	}
	obj, err := n.BackupDatabase(ctx(), dbSpec(d), up)
	if err != nil {
		return 0, err
	}
	return s.CreateBackup(ctx(), store.CreateBackupParams{
		Database: d.Name, ObjectKey: key, Parts: int64(obj.Parts), SizeBytes: obj.Size, SHA256: obj.SHA256, FileKey: secret.String(up.FileKey),
		AccountID: target.AccountID, Bucket: target.Bucket,
	})
}

// Presigned URLs: a node uploads and downloads backups with URLs for one
// part each, valid for urlTTL, which the panel signs with the S3 keys of
// the bucket's account's token. The keys reach every bucket of the
// account, so they never leave the panel.
const urlTTL = time.Hour

// newUpload is a new backup at key in t's bucket: its own key, and URLs
// to put its parts.
func newUpload(s *store.Store, t backupTarget, key string) (node.Upload, error) {
	fileKey, err := secret.NewFileKey()
	if err != nil {
		return node.Upload{}, err
	}
	sign, err := presigner(s, t, http.MethodPut)
	if err != nil {
		return node.Upload{}, err
	}
	return node.Upload{FileKey: fileKey, PartSize: backupPartSize, URL: func(part int) (string, error) {
		return sign(partKey(key, part))
	}}, nil
}

// backupDownload is a backup's parts in t's bucket, for its node to
// download, check and open.
func backupDownload(s *store.Store, t backupTarget, key string, parts int64, sha, fileKey string) (node.Download, error) {
	sign, err := presigner(s, t, http.MethodGet)
	if err != nil {
		return node.Download{}, err
	}
	return node.Download{FileKey: fileKey, SHA256: sha, Parts: int(parts), URL: func(part int) (string, error) {
		return sign(partKey(key, part))
	}}, nil
}

// presigner signs URLs for method on objects of t's bucket.
func presigner(s *store.Store, t backupTarget, method string) (func(object string) (string, error), error) {
	c, account, bucket, err := r2At(s, t)
	if err != nil {
		return nil, err
	}
	keyID, secretKey, err := r2Credentials(c)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf(s3.R2Endpoint, account)
	return func(object string) (string, error) {
		return s3.Presign(endpoint, "auto", keyID, secretKey, method, "/"+bucket+"/"+object, urlTTL, time.Now()), nil
	}, nil
}

var r2Keys struct {
	sync.Mutex
	byToken map[string][2]string
}

// r2Credentials are the S3 keys of c's token, looked up once per token.
func r2Credentials(c cloudflare.Client) (keyID, secretKey string, err error) {
	r2Keys.Lock()
	defer r2Keys.Unlock()
	if k, ok := r2Keys.byToken[c.Token]; ok {
		return k[0], k[1], nil
	}
	if keyID, secretKey, err = c.R2Credentials(); err != nil {
		return "", "", err
	}
	if r2Keys.byToken == nil {
		r2Keys.byToken = map[string][2]string{}
	}
	r2Keys.byToken[c.Token] = [2]string{keyID, secretKey}
	return keyID, secretKey, nil
}

// dbBackupDownload is backupDownload for a database backup.
func dbBackupDownload(s *store.Store, b store.Backup) (node.Download, error) {
	return backupDownload(s, backupTarget{b.AccountID, b.Bucket}, b.ObjectKey, b.Parts, b.SHA256, string(b.FileKey))
}

// uploadParts uploads size bytes of r as key/000, key/001, ...
func uploadParts(s *store.Store, t backupTarget, key string, r io.ReaderAt, size int64) (parts int, err error) {
	for offset := int64(0); offset < size || parts == 0; offset += backupPartSize {
		c, acc, bucket, err := r2At(s, t)
		if err != nil {
			return 0, err
		}
		n := min(backupPartSize, size-offset)
		if err := c.PutObject(acc, bucket, partKey(key, parts), io.NewSectionReader(r, offset, n), n); err != nil {
			return 0, err
		}
		parts++
	}
	return parts, nil
}

// tmpDir holds dumps on their way to storage; it's on the data disk rather
// than /tmp, which may be a small RAM disk.
const tmpDir = "data/tmp"

// VerifyBackup downloads a backup and restores it into a scratch database
// next to the real one, then drops it. The result is saved on the backup.
func VerifyBackup(s *store.Store, id int64) error {
	b, err := s.GetBackup(ctx(), id)
	if err != nil {
		return err
	}
	tables, verifyErr := verify(s, b)
	msg := ""
	if verifyErr != nil {
		msg = verifyErr.Error()
	}
	if err := s.SetBackupVerified(ctx(), store.SetBackupVerifiedParams{
		ID: id, VerifiedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z"), VerifyError: msg, Tables: int64(tables),
	}); err != nil {
		return err
	}
	return verifyErr
}

func verify(s *store.Store, b store.Backup) (tables int, err error) {
	d, err := s.GetDatabase(ctx(), b.Database)
	if err != nil {
		return 0, err
	}
	n, err := databaseNode(s, d)
	if err != nil {
		return 0, err
	}
	dl, err := dbBackupDownload(s, b)
	if err != nil {
		return 0, err
	}
	return n.VerifyDatabaseBackup(ctx(), dbSpec(d), dl)
}

// restoreBackup replaces its database's content with a backup; a backup
// that doesn't restore changes nothing.
func restoreBackup(s *store.Store, b store.Backup) error {
	d, err := s.GetDatabase(ctx(), b.Database)
	if err != nil {
		return err
	}
	if err := ensureInPostgres(s, d); err != nil {
		return err
	}
	n, err := databaseNode(s, d)
	if err != nil {
		return err
	}
	dl, err := dbBackupDownload(s, b)
	if err != nil {
		return err
	}
	return n.RestoreDatabase(ctx(), dbSpec(d), dl)
}

// Rotation keeps the newest backups, one a week for a month, and always the
// newest backup that restored in its check.
const keepWeeks = 4

// RotateBackups deletes the backups the rotation no longer keeps, from R2
// and the list.
func RotateBackups(s *store.Store, dbName string, now time.Time) error {
	all, err := s.ListAllBackups(ctx(), dbName)
	if err != nil {
		return err
	}
	for _, b := range backupsToDrop(all, dbBackupAge, config.BackupKeep, now) {
		if err := deleteParts(s, backupTarget{b.AccountID, b.Bucket}, b.ObjectKey, b.Parts); err != nil {
			return err // retried by the next rotation
		}
		if err := s.DeleteBackup(ctx(), b.ID); err != nil {
			return err
		}
	}
	return nil
}

// deleteParts deletes a backup's parts from the bucket.
func deleteParts(s *store.Store, t backupTarget, objectKey string, parts int64) error {
	for i := range int(parts) {
		c, acc, bucket, err := r2At(s, t)
		if err != nil {
			return err
		}
		if err := c.DeleteObject(acc, bucket, partKey(objectKey, i)); err != nil {
			return err
		}
	}
	return nil
}

// dbBackupAge is what the rotation needs to know of a backup: its ID, when
// it was made and whether it restored in its check.
func dbBackupAge(b store.Backup) (id int64, createdAt string, restored bool) {
	return b.ID, b.CreatedAt, b.VerifiedAt != "" && b.VerifyError == ""
}

// backupsToDrop picks what the rotation deletes from backups, newest first.
// Backups still under the bucket's lock can't be deleted, so they're kept.
func backupsToDrop[B any](backups []B, age func(B) (int64, string, bool), keepLast int, now time.Time) []B {
	keep := map[int64]bool{}
	weeks := map[[2]int]bool{}
	cutoff := now.AddDate(0, 0, -7*keepWeeks)
	locked := now.AddDate(0, 0, -backupLockDays)
	verified := false
	for i, b := range backups {
		id, createdAt, restored := age(b)
		if i < keepLast {
			keep[id] = true
		}
		if !verified && restored {
			keep[id], verified = true, true
		}
		t, err := time.Parse("2006-01-02T15:04:05Z", createdAt)
		if err != nil {
			keep[id] = true // unknown age: never guess it's old
			continue
		}
		if t.After(locked) {
			keep[id] = true
		}
		year, week := t.ISOWeek()
		if t.After(cutoff) && !weeks[[2]int{year, week}] {
			keep[id], weeks[[2]int{year, week}] = true, true
		}
	}
	var drop []B
	for _, b := range backups {
		if id, _, _ := age(b); !keep[id] {
			drop = append(drop, b)
		}
	}
	return drop
}

// BackupDue backs up, checks and rotates every database whose last backup
// is older than config.BackupEvery. It's called every hour, so restarts of
// the agent don't postpone backups.
func BackupDue(s *store.Store) map[string]error {
	results := map[string]error{}
	dbs, err := s.ListDatabases(ctx())
	if err != nil || BackupBucket(s) == "" {
		return results
	}
	for _, d := range dbs {
		last, err := s.ListBackups(ctx(), store.ListBackupsParams{Database: d.Name, Limit: 1})
		if err == nil && len(last) > 0 {
			if t, err := time.Parse("2006-01-02T15:04:05Z", last[0].CreatedAt); err == nil && time.Since(t) < config.BackupEvery {
				continue
			}
		}
		results[d.Name] = runDBJob(d.Name, "backing up (scheduled)", func() error { return backupAndCheck(s, d.Name) })
	}
	return results
}
