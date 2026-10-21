package ops

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/link"
	"github.com/hakobudev/hakobu/internal/store"
)

func TestSharedNames(t *testing.T) {
	config.SharedDomain = "hakobu.app"
	t.Cleanup(func() { config.SharedDomain = "" })
	for _, name := range []string{"shop", "abc", "my-shop", "a1b", "123", strings.Repeat("a", 63)} {
		if err := checkSharedName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{
		"", "ab", "-shop", "shop-", "Shop", "sh_op", "sh.op", "шоп", strings.Repeat("a", 64),
		"xn--80ak6aa92e", "ab--cd", // DNS's encoded names
		"www", "dash", "docs", "api", "admin", "mail", "status", "login", "auth", "cdn", "hakobu",
	} {
		if err := checkSharedName(name); err == nil {
			t.Errorf("%q passed", name)
		}
	}
}

// sharedSetup is a panel-only panel giving out addresses in panel.com,
// the panel's zone, to user friend, unmarked yet, with a server of theirs.
func sharedSetup(t *testing.T) (*store.Store, int64) {
	t.Helper()
	newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	config.PanelOnly, config.SharedDomain = true, "panel.com"
	t.Cleanup(func() { config.PanelOnly, config.SharedDomain = false, "" })
	friend, err := s.CreateUser(ctx(), store.CreateUserParams{GitHubID: 2, GitHubLogin: "friend"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNode(ctx(), store.CreateNodeParams{Name: "box", UserID: friend}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.GetNodeByName(ctx(), store.GetNodeByNameParams{Name: "box", UserID: friend})
	key, _ := link.NewKey()
	if err := s.JoinNode(ctx(), store.JoinNodeParams{PublicKey: fmt.Sprintf("%x", link.Public(key)), ID: n.ID}); err != nil {
		t.Fatal(err)
	}
	return s, friend
}

func TestSharedDomainNeedsTheMark(t *testing.T) {
	s, friend := sharedSetup(t)
	if _, err := CreateProjectOn(s, friend, "web", "box", ""); !errors.Is(err, errNotShared) {
		t.Fatalf("unmarked: %v, want errNotShared", err)
	}
	if err := SetUserSharedDomain(s, friend, true); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProjectOn(s, friend, "web", "", ""); !errors.Is(err, errPanelOnly) {
		t.Errorf("on the panel's server: %v, want errPanelOnly", err)
	}
	if _, err := CreateProjectOn(s, friend, "web", "box", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "web")
	if a, err := projectAccount(s, p); err != nil || !a.isPanel() {
		t.Errorf("account = %q (%v), want the panel's", a.Name, err)
	}
	if zones, def, err := ProjectZones(s, p); err != nil || len(zones) != 1 || zones[0] != "panel.com" || def != "panel.com" {
		t.Errorf("zones = %v, %q (%v), want panel.com alone", zones, def, err)
	}

	for domain, want := range map[string]string{
		"shop.panel.com":   "",
		"":                 "",
		"web.acme.com":     "isn't yours",
		"panel.com":        "isn't yours",
		"a.shop.panel.com": "isn't yours",
		"www.panel.com":    "kept for the panel",
		"ab.panel.com":     "too short",
	} {
		err := checkSharedAddress(s, p, domain)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want %q", domain, err, want)
		}
	}
	if _, err := CreateAppWith(s, store.CreateAppParams{ProjectID: p.ID, Name: "web", BuildStrategy: "dockerfile"}, "web.acme.com", ""); err == nil {
		t.Error("an app took a domain of the panel's beyond the shared one")
	}
	if _, err := s.GetAppByDomain(ctx(), "web.acme.com"); err == nil {
		t.Error("the app was left behind")
	}
	if err := createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: "api", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	app, _ := s.GetAppByID(ctx(), appID(t, s, "api"))
	if err := SetAppDomain(s, app, "api.panel.com.evil.com"); err == nil || !strings.Contains(err.Error(), "isn't yours") {
		t.Errorf("SetAppDomain: %v", err)
	}
	if _, err := CreateStorage(s, store.Storage{ProjectID: p.ID, Name: "files", Provider: "r2"}); err == nil {
		t.Error("an R2 storage in the panel's account")
	}
	if names, err := R2Buckets(s, p); err != nil || len(names) > 0 {
		t.Errorf("R2Buckets = %v (%v), want none of the panel's", names, err)
	}

	// Taken back: what they have stays, nothing new.
	if err := SetUserSharedDomain(s, friend, false); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProjectOn(s, friend, "web2", "box", ""); !errors.Is(err, errNotShared) {
		t.Errorf("after the mark is taken: %v", err)
	}
	if err := checkSharedAddress(s, p, "shop.panel.com"); !errors.Is(err, errNotShared) {
		t.Errorf("a new address after the mark is taken: %v", err)
	}
	if err := checkSharedAddress(s, p, ""); err != nil {
		t.Errorf("making an app private after the mark is taken: %v", err)
	}
}

func TestSharedDomainOff(t *testing.T) {
	s, friend := sharedSetup(t)
	if err := SetUserSharedDomain(s, friend, true); err != nil {
		t.Fatal(err)
	}
	config.SharedDomain = ""
	if _, err := CreateProjectOn(s, friend, "web", "box", ""); !errors.Is(err, errNotShared) {
		t.Errorf("without HAKOBU_SHARED_DOMAIN: %v", err)
	}
	config.SharedDomain = "hakobu.app" // not a zone the panel's token reaches
	if _, err := CreateProjectOn(s, friend, "web", "box", ""); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "web")
	if _, _, err := ProjectZones(s, p); err == nil || !strings.Contains(err.Error(), "doesn't reach hakobu.app") {
		t.Errorf("ProjectZones: %v", err)
	}
	config.PanelOnly = false // a panel of the owner's: their own projects keep every domain
	if zones, _, err := ProjectZones(s, p); err != nil || len(zones) != 1 || zones[0] != "panel.com" {
		t.Errorf("zones = %v (%v)", zones, err)
	}
}
