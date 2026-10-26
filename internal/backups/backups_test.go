package backups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/s3"
)

// memStore is a bucket in memory; locked keys refuse deletion, failPut
// refuses writes.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	locked  func(key string) bool
	failPut error
}

func newMem() *memStore { return &memStore{objects: map[string][]byte{}} }

func (s *memStore) Put(_ context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil {
		return s.failPut
	}
	s.objects[key] = bytes.Clone(body)
	return nil
}

func (s *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: no such key", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked != nil && s.locked(key) {
		return fmt.Errorf("%w: 409", s3.ErrLocked)
	}
	delete(s.objects, key)
	return nil
}

// fakeSource dumps a fixed content; a check reads it all back.
type fakeSource struct {
	content  []byte
	checkErr error
	loaded   []byte
}

func (f *fakeSource) DumpDatabase(_ context.Context, _ node.DBSpec, w io.Writer) error {
	_, err := w.Write(f.content)
	return err
}

func (f *fakeSource) CheckDump(_ context.Context, _ node.DBSpec, r io.Reader) (int, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(b, f.content) {
		return 0, errors.New("the dump came back different")
	}
	return 3, f.checkErr
}

func (f *fakeSource) LoadDump(_ context.Context, _ node.DBSpec, r io.Reader) error {
	b, err := io.ReadAll(r)
	f.loaded = b
	return err
}

func (f *fakeSource) ArchiveVolume(_ context.Context, _, _ string, w io.Writer) error {
	_, err := w.Write(f.content)
	return err
}

func (f *fakeSource) CheckArchive(r io.Reader) (int, error) {
	_, err := io.Copy(io.Discard, r)
	return 7, err
}

func (f *fakeSource) LoadArchive(_ context.Context, _ node.AppSpec, _ string, r io.Reader, _ io.Writer) error {
	b, err := io.ReadAll(r)
	f.loaded = b
	return err
}

type kit struct {
	v             *Vault
	src           *fakeSource
	primary, copy *memStore
	server, owner *age.X25519Identity
	now           time.Time
}

func newKit(t *testing.T) *kit {
	server, _ := age.GenerateX25519Identity()
	owner, _ := age.GenerateX25519Identity()
	k := &kit{src: &fakeSource{content: bytes.Repeat([]byte("row;"), 1000)}, primary: newMem(), copy: newMem(), server: server, owner: owner,
		now: time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)}
	k.v = &Vault{
		Source: k.src, Primary: Target{Name: "r2", Store: k.primary, Prefix: "hakobu/nova-1/"},
		Copies:     []Target{{Name: "hetzner", Store: k.copy, Prefix: "hakobu/nova-1/"}},
		Recipients: []string{server.Recipient().String(), owner.Recipient().String()},
		Identity:   server, PartSize: 1000, TempDir: t.TempDir(), Now: func() time.Time { return k.now },
	}
	return k
}

var db = node.DBSpec{Name: "nova_main", User: "nova_main", Password: "pw"}

func TestBackupAndRestore(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	m, err := k.v.BackupDatabase(ctx, "nova", "main", db)
	if err != nil {
		t.Fatal(err)
	}
	if m.Checked != "3 tables" || m.Parts < 2 || m.SHA256 == "" {
		t.Errorf("manifest %+v", m)
	}
	for k, b := range k.primary.objects {
		if bytes.Contains(b, []byte("row;row;")) {
			t.Errorf("%s holds the dump in the clear", k)
		}
	}
	if len(k.copy.objects) != len(k.primary.objects) {
		t.Errorf("the copy has %d objects, the primary %d", len(k.copy.objects), len(k.primary.objects))
	}
	list, err := k.v.List(ctx, "nova")
	if err != nil || len(list) != 1 || list[0].SHA256 != m.SHA256 {
		t.Fatalf("list %v, %v", list, err)
	}
	if err := k.v.RestoreDatabase(ctx, list[0], db); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.src.loaded, k.src.content) {
		t.Error("the restore loaded something else")
	}
	// The owner's key opens it too: a server lost with its key isn't the
	// end of its backups.
	k.v.Identity = k.owner
	if err := k.v.RestoreDatabase(ctx, list[0], db); err != nil {
		t.Errorf("with the owner's key: %v", err)
	}
	stranger, _ := age.GenerateX25519Identity()
	k.v.Identity = stranger
	if err := k.v.RestoreDatabase(ctx, list[0], db); err == nil || !strings.Contains(err.Error(), "isn't among them") {
		t.Errorf("with a stranger's key: %v", err)
	}
	if err := k.v.RestoreVolume(ctx, list[0], node.AppSpec{}, "data", io.Discard); err == nil {
		t.Error("a database backup was restored into a volume")
	}
}

func TestVolumeBackup(t *testing.T) {
	k := newKit(t)
	m, err := k.v.BackupVolume(context.Background(), "nova", "crm", "nova-crm", "uploads")
	if err != nil || m.Name != "crm/uploads" || m.Checked != "7 files" {
		t.Fatalf("%+v, %v", m, err)
	}
	if err := k.v.RestoreVolume(context.Background(), m, node.AppSpec{Name: "nova-crm"}, "uploads", io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedBackupIsRefused(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	m, _ := k.v.BackupDatabase(ctx, "nova", "main", db)
	for key := range k.primary.objects {
		if strings.HasSuffix(key, "part-0001") {
			k.primary.objects[key][10] ^= 1
		}
	}
	if err := k.v.RestoreDatabase(ctx, m, db); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("err = %v", err)
	}
	if k.src.loaded != nil {
		t.Error("a tampered backup was loaded")
	}
}

func TestFailedCheckLeavesNoManifest(t *testing.T) {
	k := newKit(t)
	k.src.checkErr = errors.New("pg_restore: bad")
	if _, err := k.v.BackupDatabase(context.Background(), "nova", "main", db); err == nil || !strings.Contains(err.Error(), "didn't restore") {
		t.Errorf("err = %v", err)
	}
	if list, _ := k.v.List(context.Background(), "nova"); len(list) != 0 {
		t.Errorf("an unchecked backup is listed: %v", list)
	}
	if len(k.copy.objects) != 0 {
		t.Error("an unchecked backup was copied")
	}
}

func TestFailedCopyKeepsPrimary(t *testing.T) {
	k := newKit(t)
	k.copy.failPut = errors.New("403 AccessDenied")
	m, err := k.v.BackupDatabase(context.Background(), "nova", "main", db)
	if err == nil || !strings.Contains(err.Error(), "copy to hetzner") {
		t.Errorf("err = %v", err)
	}
	if list, _ := k.v.List(context.Background(), "nova"); len(list) != 1 || list[0].SHA256 != m.SHA256 {
		t.Errorf("the primary lost it: %v", list)
	}
}

func TestBadRecipient(t *testing.T) {
	k := newKit(t)
	k.v.Recipients = append(k.v.Recipients, "ssh-ed25519 AAAA")
	if _, err := k.v.BackupDatabase(context.Background(), "nova", "main", db); err == nil {
		t.Error("a backup was sealed while a recipient was dropped")
	}
}

func TestExpired(t *testing.T) {
	day := func(d int) Manifest {
		// Wednesday 2026-10-21 and the days before it.
		return Manifest{Kind: Database, Name: "main", At: time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC).AddDate(0, 0, -d)}
	}
	var all []Manifest
	for d := range 60 {
		all = append(all, day(d))
	}
	all = append(all, Manifest{Kind: Database, Name: "other", At: day(0).At})
	// Made by hand on days already kept: they don't push a day out.
	for _, d := range []int{0, 1, 2} {
		all = append(all, Manifest{Kind: Database, Name: "main", At: day(d).At.Add(5 * time.Hour)})
	}
	gone := Expired(all, 7, 4)
	kept := map[time.Time]bool{}
	for _, m := range all {
		if m.Name == "main" {
			kept[m.At] = true
		}
	}
	for _, m := range gone {
		if m.Name == "other" {
			t.Error("the only backup of another database expired")
		}
		delete(kept, m.At)
	}
	if len(kept) != 11 {
		t.Errorf("kept %d of main, want 7 daily + 4 weekly", len(kept))
	}
	for d := range 7 {
		if !kept[day(d).At] && !kept[day(d).At.Add(5*time.Hour)] {
			t.Errorf("day %d expired", d)
		}
	}
}

func TestRotate(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	start := k.now
	for d := range 10 {
		k.now = start.AddDate(0, 0, d)
		if _, err := k.v.BackupDatabase(ctx, "nova", "main", db); err != nil {
			t.Fatal(err)
		}
	}
	// The bucket's lock refuses to delete what's under a week old.
	k.primary.locked = func(key string) bool { return strings.Contains(key, start.AddDate(0, 0, 2).Format(stamp)) }
	deleted, err := k.v.Rotate(ctx, "nova", 7, 0)
	if err != nil || len(deleted) != 2 {
		t.Errorf("deleted %v, %v", deleted, err)
	}
	if list, _ := k.v.List(ctx, "nova"); len(list) != 8 {
		t.Errorf("%d left, want 7 and the locked one", len(list))
	}
	// Still locked a month on, the lock outlasts the rotation: an error.
	k.now = start.AddDate(0, 0, 40)
	if _, err := k.v.Rotate(ctx, "nova", 7, 0); !errors.Is(err, s3.ErrLocked) {
		t.Errorf("a month on: %v", err)
	}
}

// Parts of a backup that never finished are deleted once they're a day old.
func TestRotateDropsUnfinished(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	old := k.now.Add(-48 * time.Hour).UTC().Format(stamp)
	young := k.now.Add(-time.Hour).UTC().Format(stamp)
	for _, at := range []string{old, young} {
		_ = k.primary.Put(ctx, k.v.Primary.Prefix+"nova/databases/main/"+at+"/part-0000", []byte("x"))
	}
	if _, err := k.v.BackupDatabase(ctx, "nova", "main", db); err != nil {
		t.Fatal(err)
	}
	if _, err := k.v.Rotate(ctx, "nova", 7, 4); err != nil {
		t.Fatal(err)
	}
	keys, _ := k.primary.List(ctx, k.v.Primary.Prefix+"nova/")
	joined := strings.Join(keys, " ")
	if strings.Contains(joined, old) || !strings.Contains(joined, young) || !strings.Contains(joined, "manifest.json") {
		t.Errorf("left %v", keys)
	}
}
