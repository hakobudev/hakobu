package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/s3"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
)

func TestBackupsToDrop(t *testing.T) {
	now := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	// One backup a day for 40 days, newest first; the one 20 days ago is the
	// newest that restored in its check.
	var backups []store.Backup
	for day := range 40 {
		b := store.Backup{ID: int64(40 - day), CreatedAt: now.AddDate(0, 0, -day).Format("2006-01-02T15:04:05Z")}
		if day == 20 {
			b.VerifiedAt = "x"
		}
		if day < 20 {
			b.VerifiedAt, b.VerifyError = "x", "broken"
		}
		backups = append(backups, b)
	}
	var kept []int
	dropped := backupsToDrop(backups, dbBackupAge, 7, now)
	for day, b := range backups {
		if !slices.ContainsFunc(dropped, func(d store.Backup) bool { return d.ID == b.ID }) {
			kept = append(kept, day)
		}
	}
	// Days 0-6, the newest of each older ISO week within 28 days (the
	// Sundays 9, 16 and 23 days back; 2026-09-29 is a Tuesday) and day 20,
	// the newest that restored.
	want := []int{0, 1, 2, 3, 4, 5, 6, 9, 16, 20, 23}
	if !slices.Equal(kept, want) {
		t.Errorf("kept days %v, want %v", kept, want)
	}
}

func TestDBJobs(t *testing.T) {
	release := make(chan struct{})
	if err := startDBJob("main", "backing up", func() error { <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := startDBJob("main", "restoring", func() error { return nil }); err == nil {
		t.Error("a second job should be refused while one runs")
	}
	if j := DatabaseJob("main"); j.Running != "backing up" {
		t.Errorf("running = %q", j.Running)
	}
	close(release)
	for deadline := time.Now().Add(time.Second); DatabaseJob("main").Running != ""; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
	}
	if j := DatabaseJob("main"); j.Failed || j.Last == "" {
		t.Errorf("last = %+v", j)
	}
}

// fakeR2 is the part of Cloudflare's API backups use, keeping objects in
// memory: the REST API for token "tok" of account "acc", and R2's S3 API
// for URLs presigned with that token's keys, whose signature it checks.
func fakeR2(t *testing.T) (objects map[string][]byte, lock *string) {
	objects, lock = map[string][]byte{}, new(string)
	var mu sync.Mutex
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/accounts/acc/tokens/verify" {
			fmt.Fprint(w, `{"success":true,"result":{"id":"tok-id","status":"active"}}`)
			return
		}
		if s3Path, ok := strings.CutPrefix(r.URL.Path, "/s3/acc/"); ok {
			_, key, _ := strings.Cut(s3Path, "/") // the bucket, then the key
			if !presignedRight(srv.URL+"/s3/acc", r) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			switch r.Method {
			case http.MethodPut:
				objects[key], _ = io.ReadAll(r.Body)
			case http.MethodGet:
				b, found := objects[key]
				if !found {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write(b)
			}
			return
		}
		const prefix = "/accounts/acc/r2/buckets"
		ok := `{"success":true,"errors":[],"messages":[],"result":{}}`
		path := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
		_, key, isObject := strings.Cut(path, "/objects/")
		key, _ = url.PathUnescape(key)
		switch {
		case r.Method == "POST" && path == "":
			fmt.Fprint(w, ok)
		case r.Method == "PUT" && strings.HasSuffix(path, "/lock"):
			b, _ := io.ReadAll(r.Body)
			*lock = string(b)
			fmt.Fprint(w, ok)
		case isObject && r.Method == "PUT":
			if r.ContentLength > cloudflare.MaxObjectSize {
				t.Errorf("upload of %d bytes", r.ContentLength)
			}
			objects[key], _ = io.ReadAll(r.Body)
			fmt.Fprint(w, ok)
		case isObject && r.Method == "GET":
			if b, found := objects[key]; found {
				_, _ = w.Write(b)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10007,"message":"not found"}]}`)
		case r.Method == "GET" && strings.HasSuffix(path, "/objects"):
			var keys []string
			for k := range objects {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					keys = append(keys, k)
				}
			}
			slices.Sort(keys)
			var result []map[string]string
			for _, k := range keys {
				result = append(result, map[string]string{"key": k})
			}
			b, _ := json.Marshal(map[string]any{"success": true, "result": result, "result_info": map[string]any{"is_truncated": false}})
			_, _ = w.Write(b)
		case isObject && r.Method == "DELETE":
			delete(objects, key)
			fmt.Fprint(w, ok)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old, oldS3 := cloudflare.APIURL, s3.R2Endpoint
	cloudflare.APIURL, s3.R2Endpoint = srv.URL, srv.URL+"/s3/%s"
	t.Cleanup(func() { cloudflare.APIURL, s3.R2Endpoint = old, oldS3 })
	return objects, lock
}

// presignedRight reports whether r's URL is signed with the S3 keys of
// token "tok" (ID "tok-id") for its method and path, and not expired.
func presignedRight(endpoint string, r *http.Request) bool {
	q := r.URL.Query()
	at, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	secs, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
	if time.Now().After(at.Add(time.Duration(secs) * time.Second)) {
		return false
	}
	sum := sha256.Sum256([]byte("tok"))
	path := strings.TrimPrefix(r.URL.Path, "/s3/acc")
	want := s3.Presign(endpoint, "auto", "tok-id", hex.EncodeToString(sum[:]), r.Method, path, time.Duration(secs)*time.Second, at)
	return strings.HasSuffix(want, "X-Amz-Signature="+q.Get("X-Amz-Signature"))
}

func TestR2Backups(t *testing.T) {
	objects, lock := fakeR2(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareToken(ctx(), "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}); err != nil {
		t.Fatal(err)
	}

	if _, err := uploadParts(s, backupTarget{}, "k", strings.NewReader("x"), 1); err == nil {
		t.Error("uploaded before backups were set up")
	}
	if err := SetupBackups(s); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(BackupBucket(s), "hakobu-backups-") || !strings.Contains(*lock, `"maxAgeSeconds":604800`) {
		t.Errorf("bucket %q, lock %s", BackupBucket(s), *lock)
	}

	// A dump over the part size goes up in parts and reads back whole.
	old := backupPartSize
	backupPartSize = 10
	t.Cleanup(func() { backupPartSize = old })
	dump := strings.Repeat("0123456789", 3) + "tail"
	parts, err := uploadParts(s, backupTarget{}, "main/1.sql.gz", strings.NewReader(dump), int64(len(dump)))
	if err != nil || parts != 4 || string(objects["main/1.sql.gz/003"]) != "tail" {
		t.Fatalf("parts = %d, %v, objects %v", parts, err, objects)
	}
	b := store.Backup{ObjectKey: "main/1.sql.gz", Parts: int64(parts)}

	// A node gets URLs for one part each, signed with the token's S3 keys
	// (which the fake checks), to put a new backup and to get one back.
	up, err := newUpload(s, backupTarget{}, "other/2.dump.enc")
	if err != nil || up.FileKey == "" || up.PartSize != backupPartSize {
		t.Fatalf("upload %+v, %v", up, err)
	}
	put, _ := up.URL(0)
	req, _ := http.NewRequest(http.MethodPut, put, strings.NewReader("sealed bytes"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT to the presigned URL: %v %v", resp, err)
	}
	if string(objects["other/2.dump.enc/000"]) != "sealed bytes" {
		t.Errorf("objects %v", objects)
	}
	dl, err := dbBackupDownload(s, store.Backup{ObjectKey: "other/2.dump.enc", Parts: 1, SHA256: "sum", FileKey: secret.String(up.FileKey)})
	if err != nil || dl.Parts != 1 || dl.SHA256 != "sum" || dl.FileKey != up.FileKey {
		t.Fatalf("download %+v, %v", dl, err)
	}
	get, _ := dl.URL(0)
	if resp, err := http.Get(get); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("GET of the presigned URL: %v %v", resp, err)
	}
	// A URL for one object or method reaches no other.
	if resp, err := http.Get(strings.Replace(get, "other/2.dump.enc/000", "main/1.sql.gz/000", 1)); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a URL reached another object: %v %v", resp, err)
	}
	req, _ = http.NewRequest(http.MethodPut, get, strings.NewReader("overwrite"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a GET URL was taken for a PUT: %v %v", resp, err)
	}

	// An empty dump still makes one (empty) part, so it can be read.
	if parts, _ := uploadParts(s, backupTarget{}, "empty", strings.NewReader(""), 0); parts != 1 {
		t.Errorf("empty dump: %d parts", parts)
	}

	// Rotation deletes every part of a dropped backup: this one, once seven
	// newer ones exist.
	id, err := s.CreateBackup(ctx(), store.CreateBackupParams{Database: "main", ObjectKey: b.ObjectKey, Parts: b.Parts})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		if _, err := s.CreateBackup(ctx(), store.CreateBackupParams{Database: "main", ObjectKey: fmt.Sprint("newer", i), Parts: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := RotateBackups(s, "main", time.Now().AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	for k := range objects {
		if strings.HasPrefix(k, "main/") {
			t.Errorf("%s left after rotation", k)
		}
	}
	if _, err := s.GetBackup(ctx(), id); err == nil {
		t.Error("backup still listed")
	}
}
