package ops

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/store"
)

// clientAccountStore is a panel with emails on, a user and their client's
// account, cli (token ctok, zone client.com), with no project yet.
func clientAccountStore(t *testing.T, f *fakeEmail) (s *store.Store, user store.User, account store.CloudflareAccount) {
	t.Helper()
	s = notifyStore(t)
	if err := SetupNotifications(s, "admin@example.org", "", ""); err != nil {
		t.Fatal(err)
	}
	id, err := s.CreateUser(ctx(), store.CreateUserParams{GitHubID: 2, GitHubLogin: "friend"})
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := s.CreateCloudflareAccount(ctx(), store.CreateCloudflareAccountParams{
		Name: "client", UserID: id, ApiToken: "ctok", TokenGroup: "g", AccountID: "cli", Title: "Client Co", Domains: "client.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	accountPerms.Lock()
	accountPerms.at = nil
	accountPerms.Unlock()
	user, _ = s.GetUser(ctx(), id)
	account, _ = s.GetCloudflareAccount(ctx(), accountID)
	return s, user, account
}

// A client's account alerts its owner about the projects there from
// itself: set up once there's a project and an address, its watchdog
// checking the apps there with an address, and all of it removed when
// the project goes.
func TestAccountAlerts(t *testing.T) {
	f := newFakeEmail(t)
	s, user, account := clientAccountStore(t, f)
	cli := f.client

	if err := SetUserNotify(s, user, "friend@example.org"); err != nil {
		t.Fatal(err)
	}
	if len(cli.addresses) != 0 || len(cli.routed) != 0 {
		t.Errorf("set up in an account with no project: addresses %v, routed %v", cli.addresses, cli.routed)
	}
	if _, ok := f.addresses["friend@example.org"]; !ok {
		t.Error("the address isn't confirmed in the panel's account, which sends the rest")
	}

	pid, err := s.CreateProject(ctx(), store.CreateProjectParams{Name: "shop", UserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: account.ID, Valid: true}, ID: pid}); err != nil {
		t.Fatal(err)
	}
	app := seedApp(t, s, "shop", "web")
	EnsureAccountAlerts(s)
	if _, ok := cli.addresses["friend@example.org"]; !ok || !cli.routed["mail.client.com"] {
		t.Fatalf("with a project there: addresses %v, routed %v", cli.addresses, cli.routed)
	}
	if len(cli.workers) != 0 {
		t.Errorf("a watchdog with no app to check: %v", cli.workers)
	}

	// An alert about the project comes from the client's account; one
	// about the panel from the panel's.
	before := len(f.sent())
	noteCrash(s, app, "exit 1")
	if got := cli.sent(); len(got) != 1 || !strings.Contains(got[0], "web: crashed") || len(f.sent()) != before {
		t.Errorf("crash mailed from the client's account %v, the panel's %v", got, f.sent()[before:])
	}

	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{ID: app.ID, Domain: "shop.client.com"}); err != nil {
		t.Fatal(err)
	}
	EnsureAccountAlerts(s)
	name := watchdogName()
	w, ok := cli.workers[name]
	if !ok {
		t.Fatalf("no watchdog in the client's account: %v", cli.workers)
	}
	if b := binding(w, "TARGETS"); !strings.Contains(b["text"], `"url":"https://shop.client.com/"`) || strings.Contains(b["text"], `"name":"panel"`) {
		t.Errorf("TARGETS %v", b)
	}
	if b := binding(w, "FROM"); b["text"] != "alerts@mail.client.com" {
		t.Errorf("FROM %v", b)
	}
	if b := binding(w, "TO"); b["text"] != "friend@example.org" {
		t.Errorf("TO %v", b)
	}
	if b := binding(f.workers[name], "TARGETS"); strings.Contains(b["text"], "shop.client.com") {
		t.Errorf("the panel's watchdog checks an app of a client's account: %v", b)
	}
	if info := UserNotifyOf(s, user); len(info.Accounts) != 1 || info.Accounts[0].From != "alerts@mail.client.com" || info.Accounts[0].Watching != 1 || info.Accounts[0].Verified {
		t.Errorf("Settings shows %+v", info.Accounts)
	}

	// A new address replaces the old one there, and the watchdog mails it.
	if err := SetUserNotify(s, user, "new@example.org"); err != nil {
		t.Fatal(err)
	}
	if _, old := cli.addresses["friend@example.org"]; old || len(cli.addresses) != 1 {
		t.Errorf("addresses after the change: %v", cli.addresses)
	}
	if b := binding(cli.workers[name], "TO"); b["text"] != "new@example.org" {
		t.Errorf("TO after the change: %v", b)
	}

	// The project goes: so does all of it.
	if err := s.DeleteAppCascade(ctx(), app.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{ID: pid}); err != nil {
		t.Fatal(err)
	}
	EnsureAccountAlerts(s)
	if len(cli.workers) != 0 || len(cli.kv) != 0 || len(cli.d1) != 0 || len(cli.addresses) != 0 || len(cli.routed) != 0 {
		t.Errorf("left in the client's account: workers %v, kv %v, d1 %v, addresses %v, routed %v", cli.workers, cli.kv, cli.d1, cli.addresses, cli.routed)
	}
	if _, err := s.GetAccountAlerts(ctx(), account.ID); err == nil {
		t.Error("still recorded")
	}
}

// A token without the Workers permissions still sends the emails, and
// Settings says why there's no watchdog.
func TestAccountAlertsWithoutWorkers(t *testing.T) {
	f := newFakeEmail(t)
	s, user, account := clientAccountStore(t, f)
	f.client.noWorkers = true
	pid, err := s.CreateProject(ctx(), store.CreateProjectParams{Name: "shop", UserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: account.ID, Valid: true}, ID: pid})
	app := seedApp(t, s, "shop", "web")
	_ = s.SetAppDomain(ctx(), store.SetAppDomainParams{ID: app.ID, Domain: "shop.client.com"})
	if err := SetUserNotify(s, user, "friend@example.org"); err == nil || !strings.Contains(err.Error(), "Workers Scripts Edit") {
		t.Errorf("err = %v", err)
	}
	info := UserNotifyOf(s, user)
	if len(info.Accounts) != 1 || info.Accounts[0].From == "" || !strings.Contains(info.Accounts[0].Err, "Workers") {
		t.Errorf("Settings shows %+v", info.Accounts)
	}
	noteCrash(s, app, "exit 1")
	if got := f.client.sent(); len(got) != 1 {
		t.Errorf("sent from the client's account: %v", got)
	}
}

// An admin with no address of their own gets their clients' alerts at
// the panel's, from the client's account.
func TestAccountAlertsOfAnAdmin(t *testing.T) {
	f := newFakeEmail(t)
	s, user, account := clientAccountStore(t, f)
	if err := s.SetUserAdmin(ctx(), store.SetUserAdminParams{Admin: 1, ID: user.ID}); err != nil {
		t.Fatal(err)
	}
	pid, _ := s.CreateProject(ctx(), store.CreateProjectParams{Name: "shop", UserID: user.ID})
	_ = s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: account.ID, Valid: true}, ID: pid})
	app := seedApp(t, s, "shop", "web")
	EnsureAccountAlerts(s)
	if _, ok := f.client.addresses["admin@example.org"]; !ok {
		t.Errorf("addresses in the client's account: %v", f.client.addresses)
	}
	noteOOM(s, app, "killed")
	if got := f.client.sent(); len(got) != 1 {
		t.Errorf("sent from the client's account: %v", got)
	}
}

// A server away from the panel for a few minutes is mailed to its owner,
// from their account, and so is its return; a short absence isn't.
func TestServerAwayMailed(t *testing.T) {
	f := newFakeEmail(t)
	s, user, account := clientAccountStore(t, f)
	if err := SetUserNotify(s, user, "friend@example.org"); err != nil {
		t.Fatal(err)
	}
	pid, _ := s.CreateProject(ctx(), store.CreateProjectParams{Name: "shop", UserID: user.ID})
	_ = s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: account.ID, Valid: true}, ID: pid})
	EnsureAccountAlerts(s)
	if err := s.CreateNode(ctx(), store.CreateNodeParams{Name: "t2", UserID: user.ID}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := s.ListNodes(ctx())
	id := nodes[0].ID
	if err := s.JoinNode(ctx(), store.JoinNodeParams{PublicKey: "k", ID: id}); err != nil {
		t.Fatal(err)
	}

	away := map[int64]time.Time{}
	start := time.Now()
	checkServersAway(s, away, start)
	checkServersAway(s, away, start.Add(2*time.Minute))
	if got := f.client.sent(); len(got) != 0 {
		t.Fatalf("mailed after 2 min: %v", got)
	}
	checkServersAway(s, away, start.Add(3*time.Minute))
	checkServersAway(s, away, start.Add(4*time.Minute))
	if got := f.client.sent(); len(got) != 1 || got[0] != "Server t2 isn't connected" {
		t.Fatalf("after 4 min: %v", got)
	}
	for _, g := range f.sent() {
		if strings.Contains(g, "t2") {
			t.Errorf("mailed from the panel's account: %v", g)
		}
	}

	links.mu.Lock()
	links.m[id] = &linked{}
	links.mu.Unlock()
	t.Cleanup(func() {
		links.mu.Lock()
		delete(links.m, id)
		links.mu.Unlock()
	})
	checkServersAway(s, away, start.Add(5*time.Minute))
	if got := f.client.sent(); len(got) != 2 || got[1] != "Server t2 is connected again" {
		t.Errorf("after it came back: %v", got)
	}
}
