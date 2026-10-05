package ops

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/store"
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
	"multi-tok": {
		{"id": "z-acme", "name": "acme.com", "account": map[string]string{"id": "acme-acc"}},
		{"id": "z-other", "name": "other.com", "account": map[string]string{"id": "other-acc"}},
	},
}

func newFakeAccounts(t *testing.T) *fakeAccounts {
	f := &fakeAccounts{ingress: map[string][]cloudflare.IngressRule{}, records: map[string]string{}, buckets: map[string][]string{}, objects: map[string]bool{}}
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
			ok(zones)
		case strings.HasSuffix(route, "/tokens/verify"):
			ok(map[string]string{"status": "active"})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			ok([]any{})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cfd_tunnel"):
			f.created++
			ok(map[string]string{"id": fmt.Sprintf("t-new-%d", f.created)})
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
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveCloudflareToken(ctx(), "panel-tok"))
	must(s.SaveCloudflareTunnel(ctx(), store.SaveCloudflareTunnelParams{AccountID: "acc", TunnelID: "t-panel", TunnelToken: "x"}))
	id, err := s.CreateCloudflareAccount(ctx(), store.CreateCloudflareAccountParams{Name: "acme", ApiToken: "acme-tok", AccountID: "acme-acc"})
	must(err)
	must(s.SetCloudflareAccountTunnel(ctx(), store.SetCloudflareAccountTunnelParams{TunnelID: "t-acme", TunnelToken: "y", ID: id}))
	must(s.CreateProject(ctx(), "own"))
	must(s.CreateProject(ctx(), "shop"))
	own, _ := s.GetProject(ctx(), "own")
	shop, _ := s.GetProject(ctx(), "shop")
	must(s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: id, Valid: true}, ID: shop.ID}))
	shop, _ = s.GetProject(ctx(), "shop")
	return s, own, shop
}

func TestClientTunnelsAreIsolated(t *testing.T) {
	f := newFakeAccounts(t)
	s, own, shop := accountsStore(t)
	for _, a := range []struct {
		project store.Project
		name    string
	}{{own, "web"}, {shop, "store"}} {
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: a.project.ID, Name: a.name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: a.name, ActiveSlot: "blue", LivePort: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	web, _ := s.GetApp(ctx(), "web")
	shopApp, _ := s.GetApp(ctx(), "store")

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
		"t-panel": "[{web.panel.com  http://web.hakobu:3000} {  unix:/run/hakobu/panel.sock}]",
		"t-acme":  "[{store.acme.com  http://store.hakobu:3000} {  http_status:404}]",
	}
	for tunnel, rules := range want {
		if got := fmt.Sprint(f.ingress[tunnel]); got != rules {
			t.Errorf("ingress of %s = %s, want %s", tunnel, got, rules)
		}
	}
}

func TestAddClientAccountRefuses(t *testing.T) {
	newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	for token, want := range map[string]string{
		"multi-tok": "several Cloudflare accounts",
		"panel-tok": "panel's own",
		"acme-tok":  "already connected as acme",
	} {
		if err := AddClientAccount(s, 0, "new", token); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("token %s: %v, want %q", token, err, want)
		}
	}
	if err := AddClientAccount(s, 0, "Bad Name", "acme-tok"); err == nil {
		t.Error("took an invalid name")
	}
	// The panel's token can't be swapped for one that sees a client.
	if _, err := ConnectCloudflare(s, "acme-tok"); err == nil {
		t.Error("the panel took a client's token")
	}
	if err := ReplaceClientToken(s, "acme", "panel-tok"); err == nil {
		t.Error("client acme took a token of another account")
	}
}

func TestSetProjectAccountNeedsPrivateApps(t *testing.T) {
	newFakeAccounts(t)
	s, own, _ := accountsStore(t)
	if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: own.ID, Name: "web", BuildStrategy: "dockerfile"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppDomain(ctx(), store.SetAppDomainParams{Name: "web", Domain: "web.panel.com"}); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectAccount(s, "own", "acme"); err == nil || !strings.Contains(err.Error(), "make it private") {
		t.Errorf("moved a project with a public app: %v", err)
	}
	if err := SetProjectAccount(s, "own", "nobody"); err == nil {
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
	s, _, _ := accountsStore(t)
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
			_ = removeProjectNetworks(s, p)
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
		if err := ensureProjectNetworks(s, p); err != nil {
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
	expect(panel, projectEdge("own"), projectEdge("shop"), true)
	expect(acme, projectEdge("shop"), projectEdge("own"), false)

	if err := SetProjectAccount(s, "shop", ""); err != nil {
		t.Fatal(err)
	}
	expect(panel, projectEdge("shop"), "-", true)
	expect(acme, deploy.EdgeNetwork+"-acme", projectEdge("shop"), false)
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
		must(s.CreateApp(ctx(), store.CreateAppParams{ProjectID: own.ID, Name: a.name, BuildStrategy: "dockerfile"}))
		app, _ := s.GetApp(ctx(), a.name)
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
		if app, _ := s.GetApp(ctx(), name); app.Domain != want || (want != "" && app.DnsZoneID != "z-new") {
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
		if err := CreateProjectOn(s, 0, "web", place[0], place[1]); !errors.Is(err, errPanelOnly) {
			t.Errorf("server %q, account %q: %v, want errPanelOnly", place[0], place[1], err)
		}
		if _, err := s.GetProject(ctx(), "web"); err == nil {
			t.Fatalf("server %q, account %q: the project was left behind", place[0], place[1])
		}
	}
	if err := CreateProjectOn(s, 1, "web", "box", "acme"); err == nil {
		t.Error("a user put a project on another user's server and account")
	}
	if err := CreateProjectOn(s, 0, "web", "box", "acme"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProject(ctx(), "web")
	if a, err := projectAccount(s, p); err != nil || a.Name != "acme" {
		t.Errorf("account = %q (%v), want acme", a.Name, err)
	}
	if sv, err := projectServer(s, p); err != nil || sv.Name != "box" {
		t.Errorf("server = %q (%v), want box", sv.Name, err)
	}
	if err := SetProjectServer(s, "web", ""); !errors.Is(err, errPanelOnly) {
		t.Errorf("moved to the panel's server: %v", err)
	}
	if err := SetProjectAccount(s, "web", ""); !errors.Is(err, errPanelOnly) {
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
		if err := s.CreateApp(ctx(), store.CreateAppParams{ProjectID: a.project.ID, Name: a.name, BuildStrategy: "dockerfile"}); err != nil {
			t.Fatal(err)
		}
		if a.port > 0 {
			if err := s.SetAppLive(ctx(), store.SetAppLiveParams{Name: a.name, ActiveSlot: "blue", LivePort: a.port}); err != nil {
				t.Fatal(err)
			}
		}
	}
	web, _ := s.GetApp(ctx(), "web")
	if err := SetAppDomain(s, web, "web.panel.com"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]string{{"/api", "api"}, {"api/v2/", "api"}, {"/admin", "admin"}} {
		if err := SetAppRoute(s, "web", r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range [][2]string{{"/", "api"}, {"/a b", "api"}, {"/x", "store"}, {"/x", "web"}, {"/x", "nobody"}} {
		if err := SetAppRoute(s, "web", bad[0], bad[1]); err == nil {
			t.Errorf("took route %s → %s", bad[0], bad[1])
		}
	}
	if err := SyncTunnel(s); err != nil {
		t.Fatal(err)
	}
	// admin isn't deployed: its route waits.
	want := "[{web.panel.com ^/api/v2(/|$) http://api.hakobu:8080} {web.panel.com ^/api(/|$) http://api.hakobu:8080} {web.panel.com  http://web.hakobu:3000} {  unix:/run/hakobu/panel.sock}]"
	if got := fmt.Sprint(f.ingress["t-panel"]); got != want {
		t.Errorf("ingress = %s, want %s", got, want)
	}
	if err := SetAppRoute(s, "web", "/api/v2", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAppCascade(ctx(), "api"); err != nil {
		t.Fatal(err)
	}
	if routes, _ := s.ListAppRoutes(ctx(), "web"); len(routes) != 1 || routes[0].Target != "admin" {
		t.Errorf("routes left: %v", routes)
	}
}

// "Connect with Cloudflare": the panel trades the code itself, the user
// picks the account, and the token is refreshed before it runs out.
func TestConnectWithCloudflare(t *testing.T) {
	f := newFakeAccounts(t)
	s, _, _ := accountsStore(t)
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("panel.example.com"); err != nil {
		t.Fatal(err)
	}
	config.CloudflareClientID = "cid"
	t.Cleanup(func() { config.CloudflareClientID = "" })
	var grants []string
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		grants = append(grants, r.Form.Get("grant_type"))
		switch {
		case r.Form.Get("client_id") != "cid" || (r.Form.Get("grant_type") == "authorization_code" && (r.Form.Get("redirect_uri") != "https://panel.example.com/cloudflare/callback" || r.Form.Get("code_verifier") == "")):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_request"}`)
		case r.Form.Get("code") == "good":
			fmt.Fprint(w, `{"access_token":"oauth-at-1","refresh_token":"rt-1","expires_in":3600}`)
		case r.Form.Get("refresh_token") == "rt-1":
			fmt.Fprint(w, `{"access_token":"oauth-at-2","refresh_token":"rt-2","expires_in":3600}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		}
	}))
	t.Cleanup(tokens.Close)
	old := cloudflare.TokenURL
	cloudflare.TokenURL = tokens.URL
	t.Cleanup(func() { cloudflare.TokenURL = old })

	start := func(user int64) string {
		t.Helper()
		u, err := StartCloudflareOAuth(user)
		if err != nil {
			t.Fatal(err)
		}
		q, _ := url.Parse(u)
		if q.Query().Get("client_id") != "cid" || q.Query().Get("code_challenge_method") != "S256" || !strings.Contains(q.Query().Get("scope"), "argotunnel.write") {
			t.Errorf("authorize URL %s", u)
		}
		return q.Query().Get("state")
	}
	if _, _, err := FinishCloudflareOAuth(1, start(0), "good"); err == nil {
		t.Error("another user finished the sign-in")
	}
	id, accounts, err := FinishCloudflareOAuth(0, start(0), "good")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].Name != "New Co" || strings.Join(accounts[0].Zones, ",") != "newco.com" {
		t.Errorf("accounts %+v", accounts)
	}
	if got := SuggestAccountName(s, "New Co"); got != "new-co" {
		t.Errorf("suggested %q", got)
	}
	if err := ConnectOAuthAccount(s, 1, id, "new-acc", "newco"); err == nil {
		t.Error("another user connected the account")
	}
	if err := ConnectOAuthAccount(s, 0, id, "acme-acc", "x"); err == nil {
		t.Error("connected an account the sign-in doesn't reach")
	}
	if err := ConnectOAuthAccount(s, 0, id, "new-acc", "newco"); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetCloudflareAccountByName(ctx(), "newco")
	if err != nil || row.RefreshToken != "rt-1" || row.TunnelID == "" || f.created == 0 {
		t.Fatalf("account %+v, %v", row, err)
	}

	// Its backups need an R2 token: a sign-in has no S3 keys.
	if _, err := clientAccount(s, row).s3Client(); err == nil || !strings.Contains(err.Error(), "R2 token") {
		t.Errorf("s3Client without an R2 token: %v", err)
	}

	// Close to running out, the token is refreshed and kept.
	if err := s.SetCloudflareAccountOAuth(ctx(), store.SetCloudflareAccountOAuthParams{ApiToken: row.ApiToken, RefreshToken: row.RefreshToken, TokenExpires: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), ID: row.ID}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.GetCloudflareAccountByName(ctx(), "newco")
	if a := clientAccount(s, row); a.Client.Token != "oauth-at-2" {
		t.Errorf("token %q after a refresh", a.Client.Token)
	}
	row, _ = s.GetCloudflareAccountByName(ctx(), "newco")
	if row.ApiToken != "oauth-at-2" || row.RefreshToken != "rt-2" {
		t.Errorf("refreshed tokens not kept: %+v", row)
	}
	if a := clientAccount(s, row); a.Client.Token != "oauth-at-2" || grants[len(grants)-1] != "refresh_token" || len(grants) != 2 {
		t.Errorf("a fresh token was refreshed again: %v", grants)
	}
}

// The relay Worker goes up with a fresh secret and the panel's client ID,
// on the account's workers.dev; a panel without OAuth deploys nothing.
func TestEnsureTokenRelay(t *testing.T) {
	s := notifyStore(t)
	var calls []string
	var meta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "GET" && r.URL.Path == "/accounts/acc/workers/subdomain":
			fmt.Fprint(w, `{"success":true,"result":{"subdomain":"panelco"}}`)
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/accounts/acc/workers/scripts/hakobu-token-relay-"):
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				meta = r.FormValue("metadata")
				if f, _, err := r.FormFile("worker.js"); err == nil {
					b, _ := io.ReadAll(f)
					if !strings.Contains(string(b), "dash.cloudflare.com/oauth2/token") {
						t.Error("uploaded another Worker")
					}
				}
			}
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/subdomain"):
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflare.APIURL
	cloudflare.APIURL = srv.URL
	t.Cleanup(func() { cloudflare.APIURL = old; cloudflare.SetTokenRelay("", "") })

	if err := EnsureTokenRelay(s); err != nil || len(calls) != 0 {
		t.Fatalf("without OAuth: %v, calls %v", err, calls)
	}
	config.CloudflareClientID = "cid"
	t.Cleanup(func() { config.CloudflareClientID = "" })
	if err := EnsureTokenRelay(s); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || !strings.Contains(meta, `"type":"secret_text"`) || !strings.Contains(meta, `"text":"cid"`) {
		t.Errorf("calls %v, metadata %s", calls, meta)
	}
}
