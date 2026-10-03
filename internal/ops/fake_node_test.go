package ops

import (
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

// fakeNode makes every project's node a nodetest.Fake for the test, and
// gives it app "web" in project "shop" with database "main", live in the
// blue slot on build "build-old".
func fakeNode(t *testing.T) (*store.Store, *nodetest.Fake) {
	t.Helper()
	f := nodetest.New()
	old := local
	local = f
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
