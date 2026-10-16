package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/node"
	"github.com/hakobudev/hakobu/internal/node/nodetest"
	"github.com/hakobudev/hakobu/internal/store"
)

// dataStore is notifyStore with alerts on and the panel's server a fake
// node, reached over the link's protocol.
func dataStore(t *testing.T) (*store.Store, *nodetest.Fake, *fakeEmail) {
	t.Helper()
	mail := newFakeEmail(t)
	s := notifyStore(t)
	if err := SetupNotifications(s, "me@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	f := nodetest.New()
	old := local
	local = nodetest.Linked(t, f)
	t.Cleanup(func() {
		local = old
		forgetData(nil, nil)
		dataHealth.Lock()
		dataHealth.volumes = nil
		dataHealth.Unlock()
	})
	return s, f, mail
}

func TestDatabaseDownAndBack(t *testing.T) {
	s, f, mail := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	id, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: web.ProjectID, Name: "main", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	f.DBs[store.Database{ID: id}.Key()] = "twelve bytes"

	checkData(s)
	h, ok := DatabaseHealth(id)
	if !ok || !h.Answering || h.SizeBytes != 12 {
		t.Fatalf("health %+v (%v), want answering at 12 bytes", h, ok)
	}

	f.Postgres = "isn't running (exited)"
	for range healthFailures - 1 {
		checkData(s)
	}
	if h, _ := DatabaseHealth(id); h.Answering || h.Down {
		t.Errorf("down after %d checks: %+v", healthFailures-1, h)
	}
	checkData(s)
	if h, _ := DatabaseHealth(id); !h.Down || !strings.Contains(h.Why, "Postgres isn't running") {
		t.Errorf("health %+v, want down with Postgres's cause", h)
	}
	checkData(s) // mailed once
	f.Postgres = ""
	checkData(s)
	if got, want := strings.Join(mail.sent(), "|"), "Database main: not answering|Database main: answering again"; got != want {
		t.Errorf("sent %q, want %q", got, want)
	}

	delete(f.DBs, store.Database{ID: id}.Key())
	checkData(s)
	if h, _ := DatabaseHealth(id); h.Answering || h.Why != "is missing from Postgres" {
		t.Errorf("health %+v of a database Postgres lost", h)
	}
}

func TestCacheDown(t *testing.T) {
	s, f, mail := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	id, err := CreateCache(s, web.ProjectID, "cache")
	if err != nil {
		t.Fatal(err)
	}
	key := store.Cache{ID: id}.Key()
	checkData(s)
	if h, ok := CacheHealth(id); !ok || !h.Answering {
		t.Fatalf("health %+v (%v), want answering", h, ok)
	}
	f.Containers[key] = "exited"
	for range healthFailures {
		checkData(s)
	}
	if h, _ := CacheHealth(id); !h.Down || h.Why != "isn't running (exited)" {
		t.Errorf("health %+v, want down", h)
	}
	if got := mail.sent(); len(got) != 1 || got[0] != "Cache cache: not answering" {
		t.Errorf("sent %q", got)
	}
}

func TestServerAwaySaysNothing(t *testing.T) {
	s, f, mail := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	id, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: web.ProjectID, Name: "main", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	f.Fail("DataStatus", node.ErrUnreachable)
	for range healthFailures + 1 {
		checkData(s)
	}
	if _, ok := DatabaseHealth(id); ok {
		t.Error("a check recorded while the server was away")
	}
	if got := mail.sent(); len(got) != 0 {
		t.Errorf("sent %q about a server that's away", got)
	}
}

func TestVolumeSizes(t *testing.T) {
	s, f, _ := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	if err := AddVolume(s, web.ID, "data", "/app/data"); err != nil {
		t.Fatal(err)
	}
	f.Volumes[node.Volume(web.Key(), "data")] = "1234"
	f.Volumes[node.Volume("app-99", "data")] = "not ours"
	countVolumes(s)
	if n, ok := VolumeSize(web.ID, "data"); !ok || n != 4 {
		t.Errorf("size %d (%v), want 4", n, ok)
	}
	if _, ok := VolumeSize(99, "data"); ok {
		t.Error("counted a volume of no app")
	}
}

// Each check is kept; the record has the share answered, the days and the
// outages with why, a time without checks counting neither way.
func TestDataUptime(t *testing.T) {
	s, f, _ := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	id, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: web.ProjectID, Name: "main", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	f.DBs[store.Database{ID: id}.Key()] = "x"
	checkData(s) // kept by checkData itself
	if u, err := UptimeOf(s, DBTarget(id)); err != nil || u.Windows[0].Checks != 1 || u.Windows[0].Pct != 100 {
		t.Fatalf("after one check: %+v, %v", u, err)
	}

	target := DBTarget(id)
	start := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	for i := range 20 { // every 30 seconds for 10 minutes, down from 5 to 6:30
		h := DataHealth{Answering: i < 10 || i > 13}
		if !h.Answering {
			h.Why = "Postgres didn't answer within 20s"
		}
		recordDataCheck(s, target, start.Add(time.Duration(i)*30*time.Second), h)
	}
	// An hour later, after the server was away: down again, and still.
	recordDataCheck(s, target, start.Add(time.Hour), DataHealth{Why: "Postgres isn't running (exited)"})
	u, err := UptimeOf(s, target)
	if err != nil {
		t.Fatal(err)
	}
	// 22 checks, 5 of them down (the latest one, kept by checkData, answered).
	if w := u.Windows[0]; w.Checks != 22 || w.Pct > 77.28 || w.Pct < 77.27 {
		t.Errorf("24 hours %+v", w)
	}
	if len(u.Outages) != 2 {
		t.Fatalf("outages %+v", u.Outages)
	}
	if o := u.Outages[0]; o.Why != "Postgres isn't running (exited)" || o.Ongoing {
		t.Errorf("latest outage %+v: not ongoing, the newest check answered", o)
	}
	if o := u.Outages[1]; o.Why != "Postgres didn't answer within 20s" || o.Minutes() != 2 || !o.Start.Equal(start.Add(5*time.Minute)) {
		t.Errorf("first outage %+v, %d min", o, o.Minutes())
	}
	if d := u.Days[len(u.Days)-1]; d.Checks == 0 && dayOf(start).Equal(dayOf(time.Now())) {
		t.Errorf("today %+v", d)
	}

	if err := DeleteDatabase(s, id); err != nil {
		t.Fatal(err)
	}
	if u, _ := UptimeOf(s, target); u.Windows[2].Checks != 0 {
		t.Errorf("checks of a deleted database kept: %+v", u.Windows[2])
	}
}

// A server's Postgres is what the latest check of a database there
// reported, for get_metrics.
func TestPostgresHealth(t *testing.T) {
	s, f, _ := dataStore(t)
	web := seedApp(t, s, "shop", "web")
	if _, ok := PostgresHealth(s, 0); ok {
		t.Fatal("reported before any check")
	}
	id, err := s.CreateDatabase(ctx(), store.CreateDatabaseParams{ProjectID: web.ProjectID, Name: "main", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	f.DBs[store.Database{ID: id}.Key()] = "x"
	checkData(s) // a server before v0.30.2 says nothing of it
	if _, ok := PostgresHealth(s, 0); ok {
		t.Error("reported without a size")
	}
	f.PostgresVersion, f.PostgresShm, f.ShmWanted = "18.6", 64<<20, 1<<30
	checkData(s)
	if h, ok := PostgresHealth(s, 0); !ok || h.PostgresVersion != "18.6" || h.PostgresShm != 64<<20 || h.ShmWanted != 1<<30 {
		t.Errorf("%+v (%v)", h, ok)
	}
	if _, ok := PostgresHealth(s, 7); ok {
		t.Error("another server's Postgres reported")
	}
}
