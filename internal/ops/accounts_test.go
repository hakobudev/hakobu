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
		"t-panel": "[{web.panel.com http://web.hakobu:3000} { unix:/run/hakobu/panel.sock}]",
		"t-acme":  "[{store.acme.com http://store.hakobu:3000} { http_status:404}]",
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
