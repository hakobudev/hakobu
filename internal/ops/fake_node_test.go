package ops

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/store"
)

// The panel's side of deploys and rollbacks, on a node in memory: what it
// records when the node fails at each step. The Docker tests cover the
// node's side.

// The keys of fakeNode's app "web" and database "main" on their node: the
// first of each in a new store.
const (
	webID   = 1
	webKey  = "app-1"
	mainKey = "db_1"
)

// fakeNode makes every project's node a nodetest.Fake for the test,
// reached over the link's protocol as a node on another server is, and
// gives it app "web" in project "shop" with database "main", live in the
// blue slot on build "build-old".
func fakeNode(t *testing.T) (*store.Store, *nodetest.Fake) {
	t.Helper()
	f := nodetest.New()
	old := local
	local = nodetest.Linked(t, f)
	t.Cleanup(func() { local = old })
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(createProject(s, "shop"))
	p, _ := getProject(s, "shop")
	must(createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}))
	db, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: p.ID, Name: "main", Password: "pw"})
	must(err)
	must(s.SetAppLinkedDB(ctx(), store.SetAppLinkedDBParams{ID: appID(t, s, "web"), LinkedDbID: sql.NullInt64{Int64: db, Valid: true}}))
	if web := webApp(t, s); web.ID != webID || web.Key() != webKey || db != 1 {
		t.Fatalf("web is %s, main %d: the keys below are wrong", web.Key(), db)
	}
	must(s.SetAppLive(ctx(), store.SetAppLiveParams{ID: appID(t, s, "web"), ActiveSlot: "blue", LivePort: 3000}))
	f.Images[webKey] = map[node.Image]string{node.Latest: "build-old"}
	f.Containers[webKey+"-blue"], f.Runs[webKey+"-blue"], f.Proxies[webKey] = "running", "build-old", webKey+"-blue"
	f.DBs[mainKey] = "rows of now"
	return s, f
}

func webApp(t *testing.T, s *store.Store) store.App {
	t.Helper()
	app, err := getApp(s, "web")
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestRollOutRecordsTheNewSlot(t *testing.T) {
	s, f := fakeNode(t)
	f.Images[webKey][node.Next] = "build-new"
	var out strings.Builder
	if err := rollOut(s, webApp(t, s), node.Next, &out); err != nil {
		t.Fatal(err)
	}
	app := webApp(t, s)
	if app.ActiveSlot != "green" || app.LivePort != 8080 {
		t.Errorf("record says %s on %d, want green on 8080", app.ActiveSlot, app.LivePort)
	}
	if f.Proxies[webKey] != webKey+"-green" || f.Runs[webKey+"-green"] != "build-new" || f.Containers[webKey+"-blue"] != "" {
		t.Errorf("proxy %s, containers %v, runs %v", f.Proxies[webKey], f.Containers, f.Runs)
	}
	if !f.Called("EnsureProjectNetworks") {
		t.Error("the project's networks weren't ensured first")
	}
}

func TestRollOutLeavesTheLiveVersionWhenTheNodeFails(t *testing.T) {
	for _, step := range []string{"StartCandidate", "Switch"} {
		t.Run(step, func(t *testing.T) {
			s, f := fakeNode(t)
			f.Images[webKey][node.Next] = "build-new"
			f.Fail(step, errors.New("node gone"))
			if err := rollOut(s, webApp(t, s), node.Next, &strings.Builder{}); err == nil {
				t.Fatal("no error")
			}
			// The record names the slot that serves: the old one.
			if app := webApp(t, s); app.ActiveSlot != "blue" || app.LivePort != 3000 {
				t.Errorf("record says %s on %d, want blue on 3000", app.ActiveSlot, app.LivePort)
			}
			if f.Proxies[webKey] != webKey+"-blue" || f.Containers[webKey+"-green"] != "" || f.Containers[webKey+"-blue"] != "running" {
				t.Errorf("proxy %s, containers %v", f.Proxies[webKey], f.Containers)
			}
			if step == "Switch" && !f.Called("Discard") {
				t.Error("the candidate wasn't discarded")
			}
		})
	}
}

// A deploy's snapshot always belongs to the Previous build: the data it
// last ran with, or none at all.
func TestSnapshotFollowsThePreviousBuild(t *testing.T) {
	for _, kept := range []bool{true, false} {
		s, f := fakeNode(t)
		f.Images[webKey][node.Next] = "build-new"
		if !kept {
			f.Fail("Promote", node.ErrPreviousNotKept)
		}
		app := webApp(t, s)
		var out strings.Builder
		dump := takeSnapshot(s, app, &out)
		if err := rollOut(s, app, node.Next, &out); err != nil {
			t.Fatal(err)
		}
		if err := withSnapshot(s, app, dump, &out, func() error { return promote(s, app) }); err != nil {
			t.Fatal(err)
		}
		f.DropDump(dump) // as the deploy's defer
		app = webApp(t, s)
		snap, has := f.Dumps[node.SnapshotOf(webKey)]
		switch {
		case kept && (!has || snap != "rows of now" || app.SnapshotDbID.Int64 != 1 || f.Images[webKey][node.Previous] != "build-old"):
			t.Errorf("kept: snapshot %q (%v) of %v, previous %q", snap, has, app.SnapshotDbID, f.Images[webKey][node.Previous])
		case !kept && (has || app.SnapshotDbID.Valid):
			t.Errorf("Previous wasn't kept, yet the snapshot %q of %v is", snap, app.SnapshotDbID)
		}
		if len(f.Dumps) != map[bool]int{true: 1, false: 0}[kept] {
			t.Errorf("dumps left: %v", f.Dumps)
		}
	}
}

// Rollback with data: the data goes back with the code, and both come
// back when the previous version doesn't start.
func TestRollbackWithData(t *testing.T) {
	for _, starts := range []bool{true, false} {
		s, f := fakeNode(t)
		f.Images[webKey][node.Previous] = "build-before"
		f.Dumps[node.SnapshotOf(webKey)] = "rows before the deploy"
		if err := s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{ID: appID(t, s, "web"), SnapshotDbID: sql.NullInt64{Int64: 1, Valid: true}, SnapshotAt: "2026-10-03T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		if !starts {
			f.Fail("StartCandidate", errors.New("crashed while starting"))
		}
		if err := StartRollback(s, appID(t, s, "web"), true); err != nil {
			t.Fatal(err)
		}
		waitJob(t, webID)
		logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppID: webID, Limit: 1})
		if starts {
			if logs[0].Status != "success" || f.DBs[mainKey] != "rows before the deploy" || f.Proxies[webKey] != webKey+"-green" ||
				f.Images[webKey][node.Latest] != "build-before" || f.Images[webKey][node.Previous] != "build-old" ||
				f.Dumps[node.SnapshotOf(webKey)] != "rows of now" {
				t.Errorf("rolled back: log %s, db %q, proxy %s, images %v, dumps %v", logs[0].Status, f.DBs[mainKey], f.Proxies[webKey], f.Images[webKey], f.Dumps)
			}
		} else {
			if logs[0].Status != "failed" || f.DBs[mainKey] != "rows of now" || f.Containers[webKey+"-blue"] != "running" ||
				f.Images[webKey][node.Latest] != "build-old" || f.Dumps[node.SnapshotOf(webKey)] != "rows before the deploy" {
				t.Errorf("failed rollback: log %s, db %q, containers %v, images %v, dumps %v", logs[0].Status, f.DBs[mainKey], f.Containers, f.Images[webKey], f.Dumps)
			}
		}
		if len(f.Dumps) != 1 {
			t.Errorf("dumps left: %v", f.Dumps)
		}
	}
}

func waitJob(t *testing.T, app int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); IsDeploying(app); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}

// A database's and a volume's backup go from their node to R2 and back:
// the panel only signs URLs and keeps each backup's SHA-256 and the
// fingerprint of the node's key that sealed it.
func TestBackupsGoBetweenTheNodeAndR2(t *testing.T) {
	s, f := fakeNode(t)
	fakeR2(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(s.SetBackupBucket(ctx(), "hakobu-backups-1"))

	id, err := BackupDatabase(s, dbID(t, s, "main"))
	must(err)
	b, err := s.GetBackup(ctx(), id)
	must(err)
	if b.KeyFingerprint == "" || b.SHA256 == "" || b.AccountID != "acc" || b.Bucket != "hakobu-backups-1" {
		t.Errorf("backup %+v", b)
	}
	if tables, err := verify(s, b); err != nil || tables != 1 {
		t.Errorf("verify: %d tables, %v", tables, err)
	}
	f.DBs[mainKey] = "lost"
	must(restoreBackup(s, b))
	if f.DBs[mainKey] != "rows of now" {
		t.Errorf("restored %q", f.DBs[mainKey])
	}

	// A volume is held like a deploy while it's backed up.
	must(s.AddVolume(ctx(), store.AddVolumeParams{AppID: webID, Name: "data", MountPath: "/data"}))
	f.Volumes[node.Volume(webKey, "data")] = "files"
	vid, err := BackupVolume(s, appID(t, s, "web"), "data")
	must(err)
	vb, err := s.GetVolumeBackup(ctx(), vid)
	must(err)
	must(VerifyVolumeBackup(s, vid))
	f.Volumes[node.Volume(webKey, "data")] = "broken"
	must(restoreVolume(s, webApp(t, s), vb, &strings.Builder{}))
	if f.Volumes[node.Volume(webKey, "data")] != "files" {
		t.Errorf("volume restored as %q", f.Volumes[node.Volume(webKey, "data")])
	}
	if IsDeploying(webID) {
		t.Error("the app is still held after its volume's backup")
	}
}

// The link drops while the node starts a new version: the panel can't
// know how far it got, so it keeps naming the old slot, and once the node
// is back, Reconcile removes the candidate it left.
func TestLinkDropsMidRollOut(t *testing.T) {
	s, f := fakeNode(t)
	r, closeLink, err := nodetest.Link(f)
	if err != nil {
		t.Fatal(err)
	}
	local = r
	f.Images[webKey][node.Next] = "build-new"
	f.OnCall("StartCandidate", closeLink)
	err = rollOut(s, webApp(t, s), node.Next, &strings.Builder{})
	if !errors.Is(err, node.ErrUnreachable) {
		t.Fatalf("rollOut: %v, want ErrUnreachable", err)
	}
	// The server goes on with the call after the link drops, as a real one
	// does: wait for it to finish (Called holds the fake's lock until then).
	for deadline := time.Now().Add(5 * time.Second); !f.Called("StartCandidate"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the server never got StartCandidate")
		}
	}
	if app := webApp(t, s); app.ActiveSlot != "blue" || f.Proxies[webKey] != webKey+"-blue" {
		t.Errorf("record %s, proxy %s: want both on blue", app.ActiveSlot, f.Proxies[webKey])
	}
	if f.Containers[webKey+"-green"] != "running" {
		t.Fatalf("the fake didn't start the candidate: %v", f.Containers)
	}
	local = nodetest.Linked(t, f) // back
	ReconcileSlots(s)
	if _, left := f.Containers[webKey+"-green"]; left || f.Containers[webKey+"-blue"] != "running" {
		t.Errorf("after Reconcile: %v", f.Containers)
	}
}

// The server drops off once the new version serves but before it's the
// Latest build there: the deploy counts, and the build becomes Latest as
// the server connects again, whether or not it had promoted it already.
func TestServerDropsAfterTheSwitch(t *testing.T) {
	for _, promotedFirst := range []bool{false, true} {
		s, f := fakeNode(t)
		if err := s.CreateNode(ctx(), store.CreateNodeParams{Name: "far"}); err != nil {
			t.Fatal(err)
		}
		r, closeLink, err := nodetest.Link(f)
		if err != nil {
			t.Fatal(err)
		}
		far, _ := s.GetNodeByName(ctx(), store.GetNodeByNameParams{Name: "far"})
		links.mu.Lock()
		links.m[far.ID] = &linked{remote: r.(*node.Remote)}
		links.mu.Unlock()
		shop, _ := getProject(s, "shop")
		if err := s.SetProjectNode(ctx(), store.SetProjectNodeParams{NodeID: sql.NullInt64{Int64: far.ID, Valid: true}, ID: shop.ID}); err != nil {
			t.Fatal(err)
		}
		f.Images[webKey][node.Next] = "build-new"
		if promotedFirst {
			f.OnCall("Promote", func() { go closeLink() }) // the answer is lost, the work done
		} else {
			f.OnCall("Retire", closeLink)
		}
		app := webApp(t, s)
		var out strings.Builder
		if err := rollOut(s, app, node.Next, &out); err != nil {
			t.Fatal(err)
		}
		err = withSnapshot(s, app, "", &out, func() error { return promote(s, app) })
		if !promotedFirst && !errors.Is(err, node.ErrUnreachable) {
			t.Fatalf("promote on a server gone: %v", err)
		}
		if err := s.AddPendingPromotion(ctx(), webID); err != nil {
			t.Fatal(err)
		}
		f.OnCall("Retire", nil)
		f.OnCall("Promote", nil)
		back, closeBack, err := nodetest.Link(f)
		if err != nil {
			t.Fatal(err)
		}
		finishPromotions(s, server{ID: far.ID, Name: "far"}, back)
		closeBack()
		if f.Images[webKey][node.Latest] != "build-new" || f.Images[webKey][node.Next] != "" {
			t.Errorf("promoted first %v: images %v", promotedFirst, f.Images[webKey])
		}
		if left, _ := s.ListPendingPromotions(ctx()); len(left) != 0 {
			t.Errorf("promoted first %v: still pending %v", promotedFirst, left)
		}
		links.mu.Lock()
		delete(links.m, far.ID)
		links.mu.Unlock()
	}
}

// A file uploaded into a volume: the volume is backed up first, the file
// reaches the node through the bucket, and its copy there is deleted.
func TestUploadIntoAVolume(t *testing.T) {
	s, f := fakeNode(t)
	objects, _ := fakeR2(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t"}))
	must(s.SetBackupBucket(ctx(), "hakobu-backups-1"))
	must(s.AddVolume(ctx(), store.AddVolumeParams{AppID: webID, Name: "data", MountPath: "/data"}))
	f.Volumes[node.Volume(webKey, "data")] = "files"

	file := func(content string) string {
		p := filepath.Join(t.TempDir(), "upload")
		must(os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	for _, bad := range []string{"../x.db", "/", "a/../b", "a b"} {
		src := file("x")
		if err := StartVolumeUpload(s, appID(t, s, "web"), "data", bad, src); err == nil {
			t.Errorf("took path %q", bad)
		}
		if _, err := os.Stat(src); err == nil {
			t.Errorf("%q: the upload was left on disk", bad)
		}
	}

	src := file("SQLite format 3")
	must(StartVolumeUpload(s, appID(t, s, "web"), "data", "db/app.db", src))
	waitJobs(t, webID)
	if got := f.Volumes[node.Volume(webKey, "data")+"/db/app.db"]; got != "SQLite format 3" {
		logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppID: webID, Limit: 1})
		t.Errorf("the volume has %q; log %+v", got, logs)
	}
	if backups, _ := s.ListVolumeBackups(ctx(), store.ListVolumeBackupsParams{AppID: webID, Volume: "data", Limit: 5}); len(backups) != 1 {
		t.Errorf("%d backups of the volume before the upload, want 1", len(backups))
	}
	for key := range objects {
		if strings.HasPrefix(key, "uploads/") {
			t.Errorf("the uploaded copy %s is still in the bucket", key)
		}
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("the upload was left on disk")
	}
}

func waitJobs(t *testing.T, app int64) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); IsDeploying(app); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}
