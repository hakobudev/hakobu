package ops

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hakobudev/hakobu/internal/cloudflare"
	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/deploy"
	"github.com/hakobudev/hakobu/internal/store"
)

// fakeAccounts is Cloudflare with the panel's account and a client's, each
// reachable only with its own token.
type fakeAccounts struct {
	mu      sync.Mutex
	ingress map[string][]cloudflare.IngressRule // by tunnel
	records map[string]string                   // host → CNAME target
	buckets map[string][]string                 // by account
	objects map[string]bool                     // account/bucket/key
	created int                                 // tunnels
	deleted []string                            // tunnels
	names   map[string]string                   // tunnel → its name
}

var fakeTokens = map[string][]map[string]any{
	"panel-tok": {{"id": "z-panel", "name": "panel.com", "account": map[string]string{"id": "acc"}}},
	"acme-tok":  {{"id": "z-acme", "name": "acme.com", "account": map[string]string{"id": "acme-acc"}}},
	"lapsed-tok": { // panel.com is gone from the account
		{"id": "z-new", "name": "new.com", "account": map[string]string{"id": "acc"}},
		{"id": "z-acme", "name": "acme.com", "account": map[string]string{"id": "acme-acc"}},
	},
	// What a "Connect with Cloudflare" sign-in reaches, before and after a
	// refresh.
	"oauth-at-1": {
		{"id": "z-new", "name": "newco.com", "account": map[string]string{"id": "new-acc", "name": "New Co"}},
		{"id": "z-other", "name": "other.com", "account": map[string]string{"id": "other-acc", "name": "Other"}},
	},
	"oauth-at-2": {
		{"id": "z-new", "name": "newco.com", "account": map[string]string{"id": "new-acc", "name": "New Co"}},
		{"id": "z-other", "name": "other.com", "account": map[string]string{"id": "other-acc", "name": "Other"}},
	},
	// A user's token for the accounts they're a member of.
	"user-tok": {
		{"id": "z-acme", "name": "acme.com", "account": map[string]string{"id": "acme-acc", "name": "Acme"}},
		{"id": "z-new", "name": "newco.com", "account": map[string]string{"id": "new-acc", "name": "New Co"}},
		{"id": "z-other", "name": "other.com", "account": map[string]string{"id": "other-acc", "name": "Other"}},
	},
	"multi-tok": {
		{"id": "z-acme", "name": "acme.com", "account": map[string]string{"id": "acme-acc"}},
		{"id": "z-other", "name": "other.com", "account": map[string]string{"id": "other-acc"}},
	},
}

func newFakeAccounts(t *testing.T) *fakeAccounts {
	f := &fakeAccounts{names: map[string]string{}, ingress: map[string][]cloudflare.IngressRule{}, records: map[string]string{}, buckets: map[string][]string{}, objects: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}) }
		zones := fakeTokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		// What the token may reach: its zones and their accounts.
		reaches := func(id string) bool {
			for _, z := range zones {
				if z["id"] == id || z["account"].(map[string]string)["id"] == id {
					return true
				}
			}
			return false
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 2 && (parts[0] == "accounts" || parts[0] == "zones") && !reaches(parts[1]) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		route := r.Method + " " + r.URL.Path
		switch {
		case route == "GET /zones":
			var in []map[string]any
			for _, z := range zones {
				if id := r.URL.Query().Get("account.id"); id == "" || z["account"].(map[string]string)["id"] == id {
					in = append(in, z)
				}
			}
			ok(in)
		case strings.HasSuffix(route, "/tokens/verify"):
			ok(map[string]string{"status": "active"})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			ok([]any{})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cfd_tunnel") && r.Header.Get("Authorization") == "Bearer notunnel-tok":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			ok([]any{})
		case r.Method == "GET" && (strings.HasSuffix(r.URL.Path, "/workers/scripts") || strings.HasSuffix(r.URL.Path, "/email/routing/addresses") ||
			strings.HasSuffix(r.URL.Path, "/storage/kv/namespaces") || strings.HasSuffix(r.URL.Path, "/d1/database")):
			ok([]any{}) // the token's permissions checked
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			f.created++
			id := fmt.Sprintf("t-new-%d", f.created)
			var t struct{ Name string }
			_ = json.Unmarshal(body, &t)
			f.names[id] = t.Name
			ok(map[string]string{"id": id})
		case len(parts) == 4 && parts[2] == "cfd_tunnel" && r.Method == "GET":
			ok(map[string]string{"id": parts[3], "name": f.names[parts[3]]})
		case len(parts) == 4 && parts[2] == "cfd_tunnel" && r.Method == "PATCH":
			var t struct{ Name string }
			_ = json.Unmarshal(body, &t)
			f.names[parts[3]] = t.Name
			ok(nil)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/token"):
			ok("token-of-" + parts[3])
		case r.Method == "DELETE" && len(parts) == 4 && parts[2] == "cfd_tunnel":
			f.deleted = append(f.deleted, parts[3])
			ok(nil)
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/configurations"):
			var cfg struct {
				Config struct{ Ingress []cloudflare.IngressRule } `json:"config"`
			}
			_ = json.Unmarshal(body, &cfg)
			f.ingress[parts[3]] = cfg.Config.Ingress
			ok(nil)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/dns_records"):
			ok([]any{})
		case r.Method == "DELETE" && len(parts) == 4 && parts[2] == "dns_records":
			for host := range f.records {
				if "rec-"+host == parts[3] {
					delete(f.records, host)
				}
			}
			ok(nil)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/dns_records"):
			var rec struct{ Name, Content string }
			_ = json.Unmarshal(body, &rec)
			f.records[rec.Name] = rec.Content
			ok(map[string]string{"id": "rec-" + rec.Name})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/r2/buckets"):
			var b struct{ Name string }
			_ = json.Unmarshal(body, &b)
			f.buckets[parts[1]] = append(f.buckets[parts[1]], b.Name)
			ok(nil)
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/lock"):
			ok(nil)
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/objects/"):
			_, key, _ := strings.Cut(r.URL.Path, "/objects/")
			f.objects[parts[1]+"/"+parts[4]+"/"+key] = true
			ok(nil)
		default:
			t.Errorf("unexpected %s", route)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old })
	return f
}

// accountsStore has the panel's account with its tunnel, client acme's
// with its own, project "own" in the panel's and "shop" in acme's.
func accountsStore(t *testing.T) (*store.Store, store.Project, store.Project) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "hakobu.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A client's tunnel made on the panel's server runs a real cloudflared
	// on the local Docker: it goes with the test.
	t.Cleanup(func() {
		rows, _ := s.ListCloudflareAccounts(ctx())
		for _, r := range rows {
			_ = local.RemoveTunnel(ctx(), r.Name)
		}
	})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "panel-tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t-panel", TunnelToken: "x"}))
	id, err := s.CreateCloudflareAccount(ctx(), store.CreateCloudflareAccountParams{Name: "acme", ApiToken: "acme-tok", TokenGroup: "g-acme", AccountID: "acme-acc", Title: "Acme", Domains: "acme.com"})
	must(err)
	must(s.SetCloudflareAccountTunnel(ctx(), store.SetCloudflareAccountTunnelParams{TunnelID: "t-acme", TunnelToken: "y", ID: id}))
	must(createProject(s, "own"))
	must(createProject(s, "shop"))
	own, _ := getProject(s, "own")
	shop, _ := getProject(s, "shop")
	must(s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: id, Valid: true}, ID: shop.ID}))
	shop, _ = getProject(s, "shop")
	return s, own, shop
}

func TestClientTunnelsAreIsolated(t *testing.T) {
	f := newFakeAccounts(t)
	s, own, shop := accountsStore(t)
	for _, a := range []struct {
		project store.Project
		name    string
	}{{own, "web"}, {shop, "store"}} {
		if err := createApp(s, store.CreateAppParams{ProjectID: a.project.ID, Name: a.name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAppLive(ctx(), store.SetAppLiveParams{ID: appID(t, s, a.name), ActiveSlot: "blue", LivePort: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	web, _ := getApp(s, "web")
	shopApp, _ := getApp(s, "store")

	// A domain goes to the tunnel of its project's account, from that
	// account's domains only.
	if err := SetAppDomain(s, shopApp, "store.panel.com"); err == nil {
		t.Error("a client's app got a domain of the panel's account")
	}
	if err := SetAppDomain(s, shopApp, "store.acme.com"); err != nil {
		t.Fatal(err)
	}
	if err := SetAppDomain(s, web, "web.panel.com"); err != nil {
		t.Fatal(err)
	}
	if f.records["store.acme.com"] != "t-acme.cfargotunnel.com" || f.records["web.panel.com"] != "t-panel.cfargotunnel.com" {
		t.Errorf("records %v", f.records)
	}

	// Each tunnel routes its own projects' apps, and only the panel's
	// reaches the panel.
	if err := SyncTunnel(s); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"t-panel": "[{web.panel.com  http://" + web.Key() + ".hakobu:3000} {  unix:/run/hakobu/panel.sock}]",
		"t-acme":  "[{store.acme.com  http://" + shopApp.Key() + ".hakobu:3000} {  http_status:404}]",
	}
	for tunnel, rules := range want {
		if got := fmt.Sprint(f.ingress[tunnel]); got != rules {
			t.Errorf("ingress of %s = %s, want %s", tunnel, got, rules)
		}
	}
}

// A token connects nothing it shouldn't, and one that can't do what
// hakobu needs says so when it's pasted.
func TestAddAccountTokenRefuses(t *testing.T) {
	newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	fakeTokens["notunnel-tok"] = []map[string]any{{"id": "z-nt", "name": "nt.com", "account": map[string]string{"id": "nt-acc", "name": "NT"}}}
	t.Cleanup(func() { delete(fakeTokens, "notunnel-tok") })
	for token, want := range map[string]string{
		"panel-tok":    "the panel's own account",
		"acme-tok":     "connected by someone else",
		"notunnel-tok": "can't create tunnels",
		"nobody":       "sees no domains",
	} {
		if err := AddAccountToken(s, 3, token); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", token, err, want)
		}
	}
	// Its owner's token again changes nothing.
	if err := AddAccountToken(s, 0, "acme-tok"); err != nil {
		t.Errorf("acme's owner pasting its token again: %v", err)
	}
}

func TestSetProjectAccountNeedsPrivateApps(t *testing.T) {
	newFakeAccounts(t)
	s, own, _ := accountsStore(t)
	if err := createApp(s, store.CreateAppParams{ProjectID: own.ID, Name: "web", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{ID: appID(t, s, "web"), Domain: "web.panel.com"}); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectAccount(s, projectID(t, s, "own"), "acme"); err == nil || !strings.Contains(err.Error(), "make it private") {
		t.Errorf("moved a project with a public app: %v", err)
	}
	if err := SetProjectAccount(s, projectID(t, s, "own"), "nobody"); err == nil {
		t.Error("moved a project to an unknown client")
	}
	if err := RemoveClientAccount(s, "acme"); err == nil || !strings.Contains(err.Error(), "shop") {
		t.Errorf("removed a client that still has a project: %v", err)
	}
}

func TestClientBackupsStayInClientAccount(t *testing.T) {
	f := newFakeAccounts(t)
	s, own, shop := accountsStore(t)
	if _, err := projectBackupTarget(s, shop.ID); err == nil {
		t.Error("backed up before backups were set up")
	}
	if err := s.SetBackupBucket(ctx(), "hakobu-backups-panel"); err != nil {
		t.Fatal(err)
	}
	if got, err := projectBackupTarget(s, own.ID); err != nil || got != (backupTarget{"acc", "hakobu-backups-panel"}) {
		t.Errorf("own project's target %+v, %v", got, err)
	}
	// The client's bucket is made at its first backup, once.
	first, err := projectBackupTarget(s, shop.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := projectBackupTarget(s, shop.ID)
	if first.AccountID != "acme-acc" || !strings.HasPrefix(first.Bucket, "hakobu-backups-") || again != first || len(f.buckets["acme-acc"]) != 1 {
		t.Fatalf("targets %+v, %+v, buckets %v", first, again, f.buckets)
	}
	if accountBackupBucket(s, "acme-acc") != first.Bucket || accountBackupBucket(s, "") != "hakobu-backups-panel" {
		t.Error("accountBackupBucket doesn't follow the accounts")
	}
	if _, err := uploadParts(s, first, "db/1.dump.enc", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if !f.objects["acme-acc/"+first.Bucket+"/db/1.dump.enc/000"] {
		t.Errorf("objects %v", f.objects)
	}
	// Backups made before clients are the panel's.
	if _, acc, bucket, err := r2At(s, backupTarget{}); err != nil || acc != "acc" || bucket != "hakobu-backups-panel" {
		t.Errorf("legacy target: %s %s %v", acc, bucket, err)
	}
	if _, _, _, err := r2At(s, backupTarget{"gone-acc", "b"}); err == nil {
		t.Error("reached an account that isn't connected")
	}
}

// TestDockerClientTunnelNetworks: a client's cloudflared reaches only its
// projects' edge networks and never the panel's socket, also after a
// project moves between accounts.
func TestDockerClientTunnelNetworks(t *testing.T) {
	if os.Getenv("HAKOBU_DOCKER_TEST") == "" {
		t.Skip("set HAKOBU_DOCKER_TEST=1 to run against the local Docker")
	}
	t.Chdir(t.TempDir()) // for the panel's socket directory
	newFakeAccounts(t)
	s, own, shop := accountsStore(t)
	accounts := tunnelAccounts(s)
	if len(accounts) != 2 {
		t.Fatalf("tunnel accounts %+v", accounts)
	}
	panel, acme := accounts[0], accounts[1]
	t.Cleanup(func() {
		for _, a := range accounts {
			_ = deploy.RemoveContainer(ctx(), a.container())
		}
		for _, p := range []string{"own", "shop"} {
			if pr, err := getProject(s, p); err == nil {
				_ = removeProjectNetworks(s, pr)
			}
		}
		_ = local.RemoveTunnel(ctx(), acme.Name)
	})
	// The tokens are made up: cloudflared restarts again and again, which
	// leaves its networks and mounts as they are.
	for _, a := range accounts {
		if err := startTunnel(s, localTunnel(a)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"own", "shop"} {
		if err := ensureProjectNetworks(s, projectID(t, s, p)); err != nil {
			t.Fatal(err)
		}
	}
	on := func(a cfAccount) string {
		return dockerOut(t, "inspect", "-f", "{{range $n, $_ := .NetworkSettings.Networks}}{{$n}} {{end}}| {{range .Mounts}}{{.Destination}}{{end}}", a.container())
	}
	expect := func(a cfAccount, has, hasNot string, socket bool) {
		t.Helper()
		got := on(a)
		if !strings.Contains(got, has+" ") || strings.Contains(got, hasNot+" ") || strings.Contains(got, "/run/hakobu") != socket {
			t.Errorf("%s: %s (want %s, not %s, panel socket %v)", a.container(), got, has, hasNot, socket)
		}
	}
	expect(panel, projectEdge(own.Key()), projectEdge(shop.Key()), true)
	expect(acme, projectEdge(shop.Key()), projectEdge(own.Key()), false)

	if err := SetProjectAccount(s, projectID(t, s, "shop"), ""); err != nil {
		t.Fatal(err)
	}
	expect(panel, projectEdge(shop.Key()), "-", true)
	expect(acme, deploy.EdgeNetwork+"-acme", projectEdge(shop.Key()), false)
}

// A panel whose domain lapsed moves to another domain of its account,
// keeping its subdomain and its tunnel, and its apps move along.
func TestMovePanel(t *testing.T) {
	f := newFakeAccounts(t)
	s, own, _ := accountsStore(t)
	t.Chdir(t.TempDir()) // data/public_host, data/apps_domain
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t-panel", TunnelToken: "x", PanelZoneID: "z-panel", PanelRecordID: "rec-hakobu.panel.com"}))
	must(config.SetPublicHost("hakobu.panel.com"))
	must(config.SetAppsDomain("panel.com"))
	f.records["hakobu.panel.com"] = "t-panel.cfargotunnel.com"
	for _, a := range []struct{ name, domain string }{{"web", "web.panel.com"}, {"apex", "panel.com"}, {"elsewhere", ""}} {
		must(createApp(s, store.CreateAppParams{ProjectID: own.ID, Name: a.name, BuildStrategy: "dockerfile"}))
		app, _ := getApp(s, a.name)
		must(SetAppDomain(s, app, a.domain))
	}
	must(s.SaveCloudflareToken(ctx(), "lapsed-tok"))

	for _, to := range []string{"elsewhere.org", "acme.com"} {
		if _, err := MovePanel(s, to); err == nil {
			t.Errorf("moved the panel to %s", to)
		}
	}
	m, err := MovePanel(s, "New.com.")
	must(err)
	if m.Host != "hakobu.new.com" || len(m.Manual) != 0 || len(m.Done) != 2 {
		t.Errorf("moved: %+v", m)
	}
	for _, host := range []string{"hakobu.new.com", "web.new.com", "new.com"} {
		if f.records[host] != "t-panel.cfargotunnel.com" {
			t.Errorf("%s → %q", host, f.records[host])
		}
	}
	for name, want := range map[string]string{"web": "web.new.com", "apex": "new.com", "elsewhere": ""} {
		if app, _ := getApp(s, name); app.Domain != want || (want != "" && app.DnsZoneID != "z-new") {
			t.Errorf("app %s at %q in %s", name, app.Domain, app.DnsZoneID)
		}
	}
	cf, _ := s.GetCloudflare(ctx())
	if config.PublicHost() != "hakobu.new.com" || config.AppsDomain() != "new.com" || cf.TunnelID != "t-panel" || cf.PanelZoneID != "z-new" || cf.PanelRecordID != "rec-hakobu.new.com" {
		t.Errorf("host %s, apps domain %s, cloudflare %+v", config.PublicHost(), config.AppsDomain(), cf)
	}
}

func TestMovedDomain(t *testing.T) {
	for _, c := range []struct{ domain, want string }{
		{"old.com", "new.com"}, {"a.b.old.com", "a.b.new.com"}, {"gold.com", ""}, {"old.com.ua", ""}, {"", ""},
	} {
		if got, ok := movedDomain(c.domain, "old.com", "new.com"); got != c.want || ok != (c.want != "") {
			t.Errorf("movedDomain(%q) = %q, %v", c.domain, got, ok)
		}
	}
}

func TestPanelOnlyProjectsUseTheirOwnersPlaces(t *testing.T) {
	newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	joinServer(t, s, "box", nil)
	config.PanelOnly = true
	t.Cleanup(func() { config.PanelOnly = false })

	for _, place := range [][2]string{{"", ""}, {"box", ""}, {"", "acme"}} {
		if _, err := CreateProjectOn(s, 0, "web", place[0], place[1]); !errors.Is(err, errPanelOnly) && !errors.Is(err, errNotShared) {
			t.Errorf("server %q, account %q: %v, want errPanelOnly", place[0], place[1], err)
		}
		if _, err := getProject(s, "web"); err == nil {
			t.Fatalf("server %q, account %q: the project was left behind", place[0], place[1])
		}
	}
	if _, err := CreateProjectOn(s, 1, "web", "box", "acme"); err == nil {
		t.Error("a user put a project on another user's server and account")
	}
	if _, err := CreateProjectOn(s, 0, "web", "box", "acme"); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "web")
	if a, err := projectAccount(s, p); err != nil || a.Name != "acme" {
		t.Errorf("account = %q (%v), want acme", a.Name, err)
	}
	if sv, err := projectServer(s, p); err != nil || sv.Name != "box" {
		t.Errorf("server = %q (%v), want box", sv.Name, err)
	}
	if err := SetProjectServer(s, projectID(t, s, "web"), ""); !errors.Is(err, errPanelOnly) {
		t.Errorf("moved to the panel's server: %v", err)
	}
	if err := SetProjectAccount(s, projectID(t, s, "web"), ""); !errors.Is(err, errNotShared) {
		t.Errorf("moved to the panel's account: %v", err)
	}
}

// A route sends a path of an app's address to another app of its
// project, before the app's own rule, longest path first.
func TestAppRoutes(t *testing.T) {
	f := newFakeAccounts(t)
	s, own, shop := accountsStore(t)
	for _, a := range []struct {
		project store.Project
		name    string
		port    int64
	}{{own, "web", 3000}, {own, "api", 8080}, {own, "admin", 0}, {shop, "store", 3000}} {
		if err := createApp(s, store.CreateAppParams{ProjectID: a.project.ID, Name: a.name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if a.port > 0 {
			if err := s.SetAppLive(ctx(), store.SetAppLiveParams{ID: appID(t, s, a.name), ActiveSlot: "blue", LivePort: a.port}); err != nil {
				t.Fatal(err)
			}
		}
	}
	web, _ := getApp(s, "web")
	if err := SetAppDomain(s, web, "web.panel.com"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{{"/api", "api"}, {"api/v2/", "api"}, {"/admin", "admin"}} {
		if err := SetAppRoute(s, mustApp(t, s, "web"), r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range [][2]string{{"/", "api"}, {"/a b", "api"}, {"/x", "store"}, {"/x", "web"}, {"/x", "nobody"}} {
		if err := SetAppRoute(s, mustApp(t, s, "web"), bad[0], bad[1]); err == nil {
			t.Errorf("took route %s → %s", bad[0], bad[1])
		}
	}
	if err := SyncTunnel(s); err != nil {
		t.Fatal(err)
	}
	// admin isn't deployed: its route waits.
	apiKey, webKey := mustApp(t, s, "api").Key(), mustApp(t, s, "web").Key()
	want := "[{web.panel.com ^/api/v2(/|$) http://" + apiKey + ".hakobu:8080} {web.panel.com ^/api(/|$) http://" + apiKey + ".hakobu:8080} {web.panel.com  http://" + webKey + ".hakobu:3000} {  unix:/run/hakobu/panel.sock}]"
	if got := fmt.Sprint(f.ingress["t-panel"]); got != want {
		t.Errorf("ingress = %s, want %s", got, want)
	}
	if err := SetAppRoute(s, mustApp(t, s, "web"), "/api/v2", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAppCascade(ctx(), appID(t, s, "api")); err != nil {
		t.Fatal(err)
	}
	if routes, _ := s.ListAppRoutes(ctx(), appID(t, s, "web")); len(routes) != 1 || routes[0].TargetID != appID(t, s, "admin") {
		t.Errorf("routes left: %v", routes)
	}
}

// One token of a user's connects every account it reaches, named after
// their domains, and follows them: an account it reaches later is
// connected, one it lost is forgotten unless a project uses it. Nothing is
// left in an account until a project needs it.
func TestUserTokenConnectsItsAccounts(t *testing.T) {
	f := newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	full := fakeTokens["user-tok"]
	late := map[string]any{"id": "z-late", "name": "late.com", "account": map[string]string{"id": "late-acc", "name": "Late"}}
	t.Cleanup(func() { fakeTokens["user-tok"] = full; delete(fakeTokens, "user-tok-2") })

	if err := AddAccountToken(s, 5, "user-tok"); err != nil {
		t.Fatal(err)
	}
	newco, err := s.GetCloudflareAccountByName(ctx(), "newco")
	if err != nil || newco.ApiToken != "user-tok" || newco.Title != "New Co" || newco.Domains != "newco.com" || newco.TunnelID != "" {
		t.Fatalf("newco %+v, %v", newco, err)
	}
	if other, _ := s.GetCloudflareAccountByName(ctx(), "other"); other.TokenGroup != newco.TokenGroup {
		t.Errorf("other doesn't share newco's token: %+v", other)
	}
	if acme, _ := s.GetCloudflareAccountByName(ctx(), "acme"); acme.ApiToken != "acme-tok" {
		t.Errorf("acme, connected before, changed: %+v", acme)
	}
	if left := f.created - len(f.deleted); left != 0 {
		t.Errorf("%d tunnels left in accounts no project uses", left)
	}

	// A client invites the user: the token reaches them now.
	fakeTokens["user-tok"] = append(append([]map[string]any{}, full...), late)
	SyncAccountTokens(s)
	lateRow, err := s.GetCloudflareAccountByName(ctx(), "late")
	if err != nil || lateRow.TokenGroup != newco.TokenGroup || lateRow.Title != "Late" {
		t.Fatalf("late %+v, %v", lateRow, err)
	}

	// A project uses late; then the token loses late and other.
	if err := createProject(s, "shop2"); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "shop2")
	if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: lateRow.ID, Valid: true}, ID: p.ID}); err != nil {
		t.Fatal(err)
	}
	fakeTokens["user-tok"] = full[:2] // acme, newco
	SyncAccountTokens(s)
	if _, err := s.GetCloudflareAccountByName(ctx(), "other"); err == nil {
		t.Error("kept other, which the token lost and no project uses")
	}
	if _, err := s.GetCloudflareAccountByName(ctx(), "late"); err != nil {
		t.Error("forgot late, which a project uses")
	}

	// A new token must reach the accounts projects use, and replaces the
	// old one for all of them.
	fakeTokens["user-tok-2"] = full[:2]
	if err := ReplaceAccountToken(s, 5, "user-tok-2"); err == nil || !strings.Contains(err.Error(), "Late") {
		t.Errorf("a token without the account a project uses: %v", err)
	}
	fakeTokens["user-tok-2"] = append(append([]map[string]any{}, full[:2]...), late)
	if err := ReplaceAccountToken(s, 5, "user-tok-2"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"newco", "late"} {
		if a, _ := s.GetCloudflareAccountByName(ctx(), name); a.ApiToken != "user-tok-2" {
			t.Errorf("%s kept the old token", name)
		}
	}
	if a, _ := s.GetCloudflareAccountByName(ctx(), "acme"); a.ApiToken != "acme-tok" {
		t.Errorf("acme's own token changed: %q", a.ApiToken)
	}
}

// An account connected without a tunnel gets one on the panel's server
// with its first project's first domain, and that domain goes through it.
func TestClientTunnelOnFirstDomain(t *testing.T) {
	f := newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	if err := AddAccountToken(s, 5, "user-tok"); err != nil {
		t.Fatal(err)
	}
	newco, _ := s.GetCloudflareAccountByName(ctx(), "newco")
	if err := createProject(s, "fresh"); err != nil {
		t.Fatal(err)
	}
	p, _ := getProject(s, "fresh")
	if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: newco.ID, Valid: true}, ID: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := createApp(s, store.CreateAppParams{ProjectID: p.ID, Name: "site", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	before := f.created - len(f.deleted)
	app, _ := getApp(s, "site")
	if err := SetAppDomain(s, app, "www.newco.com"); err != nil {
		t.Fatal(err)
	}
	newco, _ = s.GetCloudflareAccountByName(ctx(), "newco")
	if f.created-len(f.deleted) != before+1 || newco.TunnelID == "" || f.records["www.newco.com"] != newco.TunnelID+".cfargotunnel.com" {
		t.Errorf("tunnels %d → %d, newco's %q, records %v", before, f.created-len(f.deleted), newco.TunnelID, f.records)
	}
	app, _ = getApp(s, "site")
	if err := SetAppDomain(s, app, "shop.newco.com"); err != nil {
		t.Fatal(err)
	}
	if f.created-len(f.deleted) != before+1 {
		t.Errorf("a second domain made another tunnel")
	}
}

// The installer's token is the panel's and, from the first admin's
// sign-in, theirs: it connects their other accounts, keeps up with them,
// and is replaced for all of them at once.
func TestPanelTokenIsTheAdmins(t *testing.T) {
	newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}
	panelZone := fakeTokens["panel-tok"][0]
	newco := fakeTokens["user-tok"][1]
	late := map[string]any{"id": "z-late", "name": "late.com", "account": map[string]string{"id": "late-acc", "name": "Late"}}
	fakeTokens["admin-tok"] = []map[string]any{panelZone, newco}
	t.Cleanup(func() { delete(fakeTokens, "admin-tok"); delete(fakeTokens, "admin-tok-2") })
	if _, err := ConnectCloudflare(s, "admin-tok"); err != nil {
		t.Fatal(err)
	}
	// The panel's domains are its account's alone.
	if zones, err := Zones(s); err != nil || len(zones) != 1 || zones[0].Name != "panel.com" {
		t.Errorf("panel's zones %v, %v", zones, err)
	}

	config.PanelOnly = true
	AdoptPanelToken(s, 1) // a panel-only panel keeps its token
	config.PanelOnly = false
	if cf, _ := s.GetCloudflare(ctx()); cf.TokenGroup != "" {
		t.Error("a panel-only panel gave its token to the admin")
	}
	AdoptPanelToken(s, 1)
	cf, _ := s.GetCloudflare(ctx())
	row, err := s.GetCloudflareAccountByName(ctx(), "newco")
	if err != nil || cf.UserID != 1 || cf.TokenGroup == "" || row.TokenGroup != cf.TokenGroup || row.UserID != 1 || row.ApiToken != "admin-tok" {
		t.Fatalf("panel %d/%q, newco %+v, %v", cf.UserID, cf.TokenGroup, row, err)
	}
	if _, err := s.GetCloudflareAccountByAccountID(ctx(), "acc"); err == nil {
		t.Error("the panel's account was connected as a client's")
	}
	if !OwnsPanelToken(s, 1) || OwnsPanelToken(s, 2) {
		t.Error("OwnsPanelToken")
	}
	AdoptPanelToken(s, 2) // only the first admin's
	if cf, _ := s.GetCloudflare(ctx()); cf.UserID != 1 {
		t.Errorf("panel's token went to user %d", cf.UserID)
	}

	// A client invites the admin.
	fakeTokens["admin-tok"] = []map[string]any{panelZone, newco, late}
	SyncAccountTokens(s)
	if r, err := s.GetCloudflareAccountByName(ctx(), "late"); err != nil || r.TokenGroup != cf.TokenGroup {
		t.Fatalf("late %+v, %v", r, err)
	}

	// Pasting it as the admin's own token replaces the panel's too, and
	// it must reach the panel's account.
	fakeTokens["admin-tok-2"] = []map[string]any{newco, late}
	if err := AddAccountToken(s, 1, "admin-tok-2"); err == nil || !strings.Contains(err.Error(), "hakobu's tunnel") {
		t.Errorf("a token without the panel's account: %v", err)
	}
	fakeTokens["admin-tok-2"] = []map[string]any{panelZone, newco, late}
	if err := ReplaceCloudflareToken(s, "admin-tok-2"); err != nil {
		t.Fatal(err)
	}
	if cf, _ := s.GetCloudflare(ctx()); cf.ApiToken != "admin-tok-2" {
		t.Errorf("panel kept %q", cf.ApiToken)
	}
	for _, name := range []string{"newco", "late"} {
		if a, _ := s.GetCloudflareAccountByName(ctx(), name); a.ApiToken != "admin-tok-2" {
			t.Errorf("%s kept the old token", name)
		}
	}
}
