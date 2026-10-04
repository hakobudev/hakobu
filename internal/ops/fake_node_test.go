package ops

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/node/nodetest"
	"github.com/x0ryz/hakobu/internal/store"
)

// The panel's side of deploys and rollbacks, on a node in memory: what it
// records when the node fails at each step. The Docker tests cover the
// node's side.

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
	must(s.CreateProject(ctx(), "shop"))
	p, _ := s.GetProject(ctx(), "shop")
	must(s.CreateApp(ctx(), store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}))
	must(s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: p.ID, Name: "main", User: "main_user", Password: "pw"}))
	must(s.SetAppLinkedDB(ctx(), store.SetAppLinkedDBParams{Name: "web", LinkedDB: "main"}))
	must(s.SetAppLive(ctx(), store.SetAppLiveParams{Name: "web", ActiveSlot: "blue", LivePort: 3000}))
	f.Images["web"] = map[node.Image]string{node.Latest: "build-old"}
	f.Containers["web-blue"], f.Runs["web-blue"], f.Proxies["web"] = "running", "build-old", "web-blue"
	f.DBs["main"] = "rows of now"
	return s, f
}

func webApp(t *testing.T, s *store.Store) store.App {
	t.Helper()
	app, err := s.GetApp(ctx(), "web")
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestRollOutRecordsTheNewSlot(t *testing.T) {
	s, f := fakeNode(t)
	f.Images["web"][node.Next] = "build-new"
	var out strings.Builder
	if err := rollOut(s, webApp(t, s), node.Next, &out); err != nil {
		t.Fatal(err)
	}
	app := webApp(t, s)
	if app.ActiveSlot != "green" || app.LivePort != 8080 {
		t.Errorf("record says %s on %d, want green on 8080", app.ActiveSlot, app.LivePort)
	}
	if f.Proxies["web"] != "web-green" || f.Runs["web-green"] != "build-new" || f.Containers["web-blue"] != "" {
		t.Errorf("proxy %s, containers %v, runs %v", f.Proxies["web"], f.Containers, f.Runs)
	}
	if !f.Called("EnsureProjectNetworks") {
		t.Error("the project's networks weren't ensured first")
	}
}

func TestRollOutLeavesTheLiveVersionWhenTheNodeFails(t *testing.T) {
	for _, step := range []string{"StartCandidate", "Switch"} {
		t.Run(step, func(t *testing.T) {
			s, f := fakeNode(t)
			f.Images["web"][node.Next] = "build-new"
			f.Fail(step, errors.New("node gone"))
			if err := rollOut(s, webApp(t, s), node.Next, &strings.Builder{}); err == nil {
				t.Fatal("no error")
			}
			// The record names the slot that serves: the old one.
			if app := webApp(t, s); app.ActiveSlot != "blue" || app.LivePort != 3000 {
				t.Errorf("record says %s on %d, want blue on 3000", app.ActiveSlot, app.LivePort)
			}
			if f.Proxies["web"] != "web-blue" || f.Containers["web-green"] != "" || f.Containers["web-blue"] != "running" {
				t.Errorf("proxy %s, containers %v", f.Proxies["web"], f.Containers)
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
		f.Images["web"][node.Next] = "build-new"
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
		snap, has := f.Dumps[node.SnapshotOf("web")]
		switch {
		case kept && (!has || snap != "rows of now" || app.SnapshotDB != "main" || f.Images["web"][node.Previous] != "build-old"):
			t.Errorf("kept: snapshot %q (%v) of %q, previous %q", snap, has, app.SnapshotDB, f.Images["web"][node.Previous])
		case !kept && (has || app.SnapshotDB != ""):
			t.Errorf("Previous wasn't kept, yet the snapshot %q of %q is", snap, app.SnapshotDB)
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
		f.Images["web"][node.Previous] = "build-before"
		f.Dumps[node.SnapshotOf("web")] = "rows before the deploy"
		if err := s.SetAppSnapshot(ctx(), store.SetAppSnapshotParams{Name: "web", SnapshotDB: "main", SnapshotAt: "2026-10-03T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		if !starts {
			f.Fail("StartCandidate", errors.New("crashed while starting"))
		}
		if err := StartRollback(s, "web", true); err != nil {
			t.Fatal(err)
		}
		waitJob(t, "web")
		logs, _ := s.ListDeployLogs(ctx(), store.ListDeployLogsParams{AppName: "web", Limit: 1})
		if starts {
			if logs[0].Status != "success" || f.DBs["main"] != "rows before the deploy" || f.Proxies["web"] != "web-green" ||
				f.Images["web"][node.Latest] != "build-before" || f.Images["web"][node.Previous] != "build-old" ||
				f.Dumps[node.SnapshotOf("web")] != "rows of now" {
				t.Errorf("rolled back: log %s, db %q, proxy %s, images %v, dumps %v", logs[0].Status, f.DBs["main"], f.Proxies["web"], f.Images["web"], f.Dumps)
			}
		} else {
			if logs[0].Status != "failed" || f.DBs["main"] != "rows of now" || f.Containers["web-blue"] != "running" ||
				f.Images["web"][node.Latest] != "build-old" || f.Dumps[node.SnapshotOf("web")] != "rows before the deploy" {
				t.Errorf("failed rollback: log %s, db %q, containers %v, images %v, dumps %v", logs[0].Status, f.DBs["main"], f.Containers, f.Images["web"], f.Dumps)
			}
		}
		if len(f.Dumps) != 1 {
			t.Errorf("dumps left: %v", f.Dumps)
		}
	}
}

func waitJob(t *testing.T, app string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); IsDeploying(app); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("job still running")
		}
	}
}

// A database's and a volume's backup go from their node to R2 and back:
// the panel only signs URLs and keeps each backup's key and SHA-256.
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

	id, err := BackupDatabase(s, "main")
	must(err)
	b, err := s.GetBackup(ctx(), id)
	must(err)
	if b.FileKey == "" || b.SHA256 == "" || b.AccountID != "acc" || b.Bucket != "hakobu-backups-1" {
		t.Errorf("backup %+v", b)
	}
	if tables, err := verify(s, b); err != nil || tables != 1 {
		t.Errorf("verify: %d tables, %v", tables, err)
	}
	f.DBs["main"] = "lost"
	must(restoreBackup(s, b))
	if f.DBs["main"] != "rows of now" {
		t.Errorf("restored %q", f.DBs["main"])
	}

	// A volume is held like a deploy while it's backed up.
	must(s.AddVolume(ctx(), store.AddVolumeParams{AppName: "web", Name: "data", MountPath: "/data"}))
	f.Volumes[node.Volume("web", "data")] = "files"
	vid, err := BackupVolume(s, "web", "data")
	must(err)
	vb, err := s.GetVolumeBackup(ctx(), vid)
	must(err)
	must(VerifyVolumeBackup(s, vid))
	f.Volumes[node.Volume("web", "data")] = "broken"
	must(restoreVolume(s, webApp(t, s), vb, &strings.Builder{}))
	if f.Volumes[node.Volume("web", "data")] != "files" {
		t.Errorf("volume restored as %q", f.Volumes[node.Volume("web", "data")])
	}
	if IsDeploying("web") {
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
	f.Images["web"][node.Next] = "build-new"
	f.OnCall("StartCandidate", closeLink)
	err = rollOut(s, webApp(t, s), node.Next, &strings.Builder{})
	if !errors.Is(err, node.ErrUnreachable) {
		t.Fatalf("rollOut: %v, want ErrUnreachable", err)
	}
	if app := webApp(t, s); app.ActiveSlot != "blue" || f.Proxies["web"] != "web-blue" {
		t.Errorf("record %s, proxy %s: want both on blue", app.ActiveSlot, f.Proxies["web"])
	}
	if f.Containers["web-green"] != "running" {
		t.Fatalf("the fake didn't start the candidate: %v", f.Containers)
	}
	local = nodetest.Linked(t, f) // back
	ReconcileSlots(s)
	if _, left := f.Containers["web-green"]; left || f.Containers["web-blue"] != "running" {
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
		links.mu.Lock()
		links.m["far"] = &linked{remote: r.(*node.Remote)}
		links.mu.Unlock()
		far, _ := s.GetNodeByName(ctx(), "far")
		shop, _ := s.GetProject(ctx(), "shop")
		if err := s.SetProjectNode(ctx(), store.SetProjectNodeParams{NodeID: sql.NullInt64{Int64: far.ID, Valid: true}, ID: shop.ID}); err != nil {
			t.Fatal(err)
		}
		f.Images["web"][node.Next] = "build-new"
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
		if err := s.AddPendingPromotion(ctx(), "web"); err != nil {
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
		if f.Images["web"][node.Latest] != "build-new" || f.Images["web"][node.Next] != "" {
			t.Errorf("promoted first %v: images %v", promotedFirst, f.Images["web"])
		}
		if left, _ := s.ListPendingPromotions(ctx()); len(left) != 0 {
			t.Errorf("promoted first %v: still pending %v", promotedFirst, left)
		}
		links.mu.Lock()
		delete(links.m, "far")
		links.mu.Unlock()
	}
}
