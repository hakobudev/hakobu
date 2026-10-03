package cmd

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/x0ryz/hakobu/internal/cloudflare"
	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/deploy"
	"github.com/x0ryz/hakobu/internal/detect"
	"github.com/x0ryz/hakobu/internal/ops"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
	"github.com/x0ryz/hakobu/internal/update"
)

// renders renders c, failing the test on an error (a missing icon, say).
func renders(t *testing.T, name string, c templ.Component) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Errorf("%s: %v", name, err)
	}
	return b.String()
}

func TestPagesRender(t *testing.T) {
	project := store.Project{ID: 1, Name: "demo", SharedEnv: "A=1"}
	app := appView{App: store.App{Name: "web", ProjectName: "demo", Repo: "o/r", Stack: "Python · FastAPI", Domain: "web.example.com", Port: 8081, ContainerPort: 8080, Env: "B=2", LinkedDB: "main", LinkedStorage: "files", MemoryMB: 512, Cpus: 0.5, SnapshotAt: "t"}, State: deploy.State{Status: "running", Restarts: 2, OOMKilled: true}}
	worker := &store.Worker{AppName: "web", Name: "worker", Command: "run", Env: "C=3"}
	db := store.Database{Name: "main", User: "main_user"}
	storages := []store.Storage{{Name: "files", Provider: "r2", Bucket: "hakobu-files-1", AccountID: "acc"}, {Name: "media", Provider: "s3", Bucket: "m", AccessKeyID: "k"}}
	deploys := []store.DeployLog{{ID: 2, Status: "running", Trigger: "push", Output: "x", CreatedAt: "2026-10-02T12:00:00Z"}, {ID: 1, Status: "failed", Trigger: "manual"}}
	events := []teldb.TelemetryEvent{{Kind: "error", Message: "boom", AppName: "web", TraceID: "abc"}, {Kind: "oom"}, {Kind: "health", Level: "error"}, {Kind: "log"}}

	pv := projectView{Project: project, Apps: []appView{app, {App: store.App{Name: "api", ProjectName: "demo"}, Deploying: true}}, Workers: map[string]workerCard{"web": {Name: "worker", Command: "taskiq worker app.tasks:broker", Status: "running"}},
		LastDeploy: map[string]store.DeployLog{"web": deploys[0]}, Databases: []store.Database{db, {Name: "other"}}, LastBackup: map[string]store.Backup{"main": {VerifiedAt: "t"}},
		Storages: storages, Sealed: []string{"P"}, SuggestedDB: "demo", BackupBucket: "hakobu-backups-1", Watchdog: true,
		Volumes: map[string][]volumeCard{"web": {{Volume: store.Volume{Name: "data", MountPath: "/data"}}, {Volume: store.Volume{Name: "up"}, HasLast: true, Last: store.VolumeBackup{VerifiedAt: "t"}}}},
		Calls:   map[string][]string{"api": {"web"}}}
	ap := appPage{App: app, Project: project, Worker: worker, WorkerStatus: "running", DataRollbackBlocker: "", LastOOM: "2026-09-29T10:00:00Z",
		Deploys: deploys, Events: events, Output: "line", Range: "24h",
		Routes:    []ops.RouteStats{{Name: "/orders/{id}", Count: 20, Errors: 1, AvgMs: 40, P50Ms: 20, P95Ms: 10000, P95Over: true}},
		Traces:    []teldb.ListTracesRow{{ID: 1, AppName: "web", Name: "/orders/{id}", Status: "internal_error", HttpStatus: 500, DurationMs: 30}},
		Effective: []ops.EnvVar{{Key: "A", Value: "1"}, {Key: "S", Sealed: true}}, SealedApp: []string{"S"}, SealedProject: []string{"P"},
		Zones: []string{"example.com"}, Sub: "web", Zone: "example.com", Databases: []store.Database{db}, Storages: storages,
		Volumes: []store.Volume{{AppName: "web", Name: "data", MountPath: "/app/data"}}, BackupBucket: "hakobu-backups-1", VolumeJobRunning: true, SealedWorker: []string{"W"},
		VolumeBackups: []volumeBackups{
			{Volume: "data", Job: ops.DBJob{Running: "backing up"}, Backups: []store.VolumeBackup{{ID: 1, ObjectKey: "k", SizeBytes: 10}, {ID: 2, VerifiedAt: "t", Files: 3}, {ID: 3, VerifiedAt: "t", VerifyError: "boom"}}},
			{Volume: "cache", Job: ops.DBJob{Last: "x", Failed: true}},
		}}
	bare := appPage{App: appView{App: store.App{Name: "web", ProjectName: "demo", LinkedDB: "main"}, State: deploy.State{Status: "exited"}}, DataRollbackBlocker: "no snapshot"}
	settings := settingsPage{PublicHost: "p", Owner: "me", GitHubSlug: "hakobu-p", Disk: "1.0 GB of 10.0 GB used (10%)", DiskLow: true, LastCleanup: "2026-09-27 12:00: freed 1.0 GB", BackupBucket: "hakobu-backups-1",
		Rotation:            ops.Rotation{Started: "2026-09-30 10:00", Log: "done    x\n", Manual: []string{"GitHub App ..."}, Failures: 1},
		CloudflareConnected: true, Notify: ops.NotifyInfo{On: true, Email: "me@example.org", From: "hakobu@mail.p"}, Watchdog: ops.WatchdogInfo{On: true, Script: "hakobu-watchdog-1a2b3c4d", Err: "boom"}, TokenURL: "https://dash.cloudflare.com/x",
		Token:       []cloudflare.Permission{{Name: "Zone Read", For: "domains"}, {Name: "Workers Scripts Edit", For: "the watchdog", Missing: true}, {Name: "Workers R2 Storage Edit", For: "backups", Unknown: true}},
		OAuthGrants: []store.OAuthGrant{{ID: 1, ClientName: "Claude", Scope: "read deploy", CreatedAt: "t", LastUsedAt: "u"}},
		Usage:       usageRows([]teldb.Sample{{Target: ops.HostTarget, Cpu: 1, CpuLimit: 4, Mem: 1 << 30, MemLimit: 8 << 30}, {Target: "app:web", Mem: 500 << 20, MemLimit: 512 << 20}, {Target: "service:postgres", Mem: 100 << 20}}),
		Update:      ops.UpdateInfo{Current: "v0.6.0", Latest: "v0.7.0", CheckedAt: "2026-10-02 12:00 UTC", Available: true, Updater: true, HasLast: true, Last: update.Status{State: "running", To: "v0.7.0", Message: "downloading"}}}

	pages := map[string]templ.Component{
		"login":            loginPage(true, "nope"),
		"login, no app":    loginPage(false, ""),
		"setup":            setupPage("p.example.com", `{"a":1}`, "s"),
		"setup, no host":   setupPage("", "", ""),
		"home":             homePage([]homeProject{{Name: "demo", Apps: 2, Down: 1, Logos: []string{"react", "fastapi", "postgresql"}}, {Name: "b", Deploying: 1}, {Name: "c"}}, []cloudflare.Permission{{Name: "Workers Scripts Edit", For: "the watchdog", Missing: true}}),
		"home, empty":      homePage(nil, nil),
		"new-app":          newAppForm(newAppData{Project: "demo", Repos: []string{"o/r"}, InstallURL: "https://x", Zones: []string{"example.com", "other.dev"}, DefaultZone: "example.com"}),
		"new-app, no zone": newAppForm(newAppData{Project: "demo", RepoError: "x"}),
		"presets":          presetsFragment([]detect.Preset{{Strategy: "dockerfile", Path: ".", Stack: "Python · FastAPI", Port: 8000}, {Strategy: "railpack", Path: "web", Stack: "Node.js"}}, ""),
		"presets error":    presetsFragment(nil, "boom"),
		"app status":       appStatus(app),
		"usage": usageFragment(usageView{Query: "target=host", Range: "24h", Ranges: usageRanges, Charts: targetCharts(ops.HostTarget, []teldb.Sample{
			{Ts: 1000, Cpu: 0.5, CpuMax: 1, CpuLimit: 2, Mem: 1 << 30, MemMax: 1 << 30, MemLimit: 4 << 30, Load: 0.3},
			{Ts: 1060, Cpu: 1.5, CpuMax: 2, CpuLimit: 2, Mem: 2 << 30, MemMax: 3 << 30, MemLimit: 4 << 30, Load: 0.9},
		}, 0, 2000)}),
		"usage, empty": usageFragment(usageView{Range: "24h", Ranges: usageRanges}),
		"database": databaseView(databasePage{DB: db, Project: project, Ready: true, Env: splitEnv([]string{"A=1"}), BackupBucket: "hakobu-backups-1", Backups: []store.Backup{{ObjectKey: "k", SizeBytes: 2048}, {ID: 2, VerifiedAt: "t", Tables: 3}, {ID: 3, VerifiedAt: "t", VerifyError: "boom"}}, UsedBy: []string{"web"},
			Keep: 7, Job: ops.DBJob{Running: "backing up"}}),
		"database, no backups": databaseView(databasePage{DB: db, Job: ops.DBJob{Last: "x", Failed: true}}),
		"settings":             settingsView(settings),
		"settings, bare":       settingsView(settingsPage{CloudflareConnected: true, Notify: ops.NotifyInfo{On: true, Err: "no token"}, Update: ops.UpdateInfo{Current: "dev"}}),
		"oauth-consent": oauthConsentPage(consentView{Client: oauthClient{ID: "https://claude.ai/oauth/claude-code-client-metadata", Name: "Claude Code"}, Query: "a=b",
			RedirectHost: "localhost:3118", Loopback: true, Document: true, Deploy: true, PublicHost: "p"}),
		"oauth-error": oauthErrorPage("boom"),
		"uptime": uptimeFragment(uptimeView{Uptime: ops.Uptime{AvgMs: 120, Windows: []ops.UptimeWindow{{Label: "24 hours", Pct: 99.5, Checks: 1440}, {Label: "7 days", Pct: 100, Checks: 10}, {Label: "30 days", Pct: 100, Checks: 10}},
			Outages: []ops.Outage{{Start: time.Unix(1000, 0), End: time.Unix(1120, 0), Status: 530, Ongoing: true}, {Start: time.Unix(0, 0), End: time.Unix(0, 0)}}}}),
		"trace": tracePage(traceView{ops.Waterfall{Trace: teldb.Trace{ID: 1, AppName: "web", Name: "/orders/{id}", DurationMs: 1500, HttpStatus: 200, Status: "ok", TraceID: "abc"},
			Rows: []ops.WaterfallRow{{Op: "http.server", Description: "/orders/{id}", WidthPct: 100, Ms: 1500}, {Op: "db", Description: "SELECT 1", Depth: 1, LeftPct: 10, WidthPct: 50, Ms: 750, Status: "internal_error"}}}, "2026-10-02 12:00:00 UTC"}),
	}
	for _, tab := range []string{"", "variables", "settings"} {
		v := pv
		v.Tab = tab
		pages["project "+tab] = projectPage(v)
	}
	pages["project, empty"] = projectPage(projectView{Project: project})
	pages["switch projects"] = switchProjects([]store.Project{project, {Name: "b"}}, "demo")
	pages["switch resources"] = switchResources("demo", []store.App{app.App, {Name: "x", BuildStrategy: "dockerfile"}}, []store.Database{db}, storages, "web")
	pages["storage"] = storageView(storagePage{Storage: storages[0], Project: project, UsedBy: []string{"web"}})
	pages["storage, keys, unused"] = storageView(storagePage{Storage: store.Storage{Name: "media", Provider: "s3", Bucket: "m", AccessKeyID: "k", Endpoint: "https://s3.example.com"}, Project: project})
	for name, page := range map[string]func(appPage) templ.Component{
		"overview": appOverview, "deployments": appDeployments, "logs": appLogs, "errors": appErrors,
		"performance": appPerformance, "variables": appVariables, "settings": appSettings,
	} {
		full, empty := ap, bare
		full.Tab, empty.Tab = name, name
		pages["app "+name] = page(full)
		pages["app "+name+", bare"] = page(empty)
	}
	logsOff := ap
	logsOff.Logs, logsOff.Range, logsOff.OfWorker, logsOff.OutputErr, logsOff.EnvError = true, "7d", true, "no container", "bad"
	for name, page := range map[string]func(appPage) templ.Component{"errors": appErrors, "performance": appPerformance, "logs": appLogs, "variables": appVariables} {
		pages["app "+name+", other branch"] = page(logsOff)
	}
	for _, st := range []string{"updated", "up to date", "failed", "rolled back"} {
		pages["settings after an update that "+st] = settingsView(settingsPage{Update: ops.UpdateInfo{Current: "v0.7.0", Latest: "v0.7.0", Updater: true, HasLast: true, Last: update.Status{State: st, From: "v0.6.0", To: "v0.7.0"},
			Previous: "v0.6.0", UpdatedAt: "t", CanRollBack: true, RollbackLosesData: st == "failed", RollbackBlocked: map[bool]string{true: "gone"}[st == "rolled back"]}})
	}
	for i, v := range []uptimeView{{NoHistory: true}, {Err: "boom"}, {}, {Uptime: ops.Uptime{Windows: []ops.UptimeWindow{{}, {}, {Pct: 97, Checks: 3}}}}} {
		pages[fmt.Sprint("uptime ", i)] = uptimeFragment(v)
		pages[fmt.Sprint("uptime badge ", i)] = uptimeBadge(v)
	}
	for i, v := range []notifyFormView{
		{PublicHost: "p", Name: "alerts", Notify: ops.NotifyInfo{On: true}, To: []ops.NotifyChoice{{Value: "a@b.c", Note: "n", Selected: true}}, From: []ops.NotifyChoice{{Value: "mail.p", Note: "n"}}},
		{PublicHost: "p", Error: "boom", OtherValue: "x@y.z", Why: []string{"example.com: its mail goes to mx.example"}},
	} {
		pages[fmt.Sprint("notify-form ", i)] = notifyForm(v)
	}
	for name, page := range pages {
		renders(t, name, page)
	}
}

func TestDomainForm(t *testing.T) {
	zones := []string{"example.com", "shop.example.com", "other.dev"}
	for domain, want := range map[string][2]string{
		"web.example.com":      {"web", "example.com"},
		"example.com":          {"", "example.com"},
		"api.shop.example.com": {"api", "shop.example.com"},
		"":                     {"", ""},
	} {
		sub, zone := splitDomain(domain, zones)
		if sub != want[0] || zone != want[1] {
			t.Errorf("splitDomain(%q) = %q, %q", domain, sub, zone)
		}
		r, _ := http.NewRequest("POST", "/", strings.NewReader(url.Values{"sub": {sub}, "zone": {zone}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if got := formDomain(r); got != domain {
			t.Errorf("formDomain(%q, %q) = %q, want %q", sub, zone, got, domain)
		}
	}
}

// A domain outside the listed zones must stay editable as-is instead of the
// form falling back to "private" and dropping it on save.
func TestDomainFieldKeepsUnknownDomain(t *testing.T) {
	b := renders(t, "domain field", domainField([]string{"example.com"}, "", "", "shop.other.net", false))
	if !strings.Contains(b, `name="domain" value="shop.other.net"`) || strings.Contains(b, `name="zone"`) {
		t.Errorf("unexpected field:\n%s", b)
	}
}

func TestPanelRejectsOtherOrigins(t *testing.T) {
	mux := http.NewServeMux()
	for _, p := range []string{"POST /settings/access", "POST /webhook/github", "POST /api/{app_id}/envelope/"} {
		mux.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	h := panelHandler(mux)
	for path, want := range map[string]int{"/settings/access": http.StatusForbidden, "/webhook/github": http.StatusOK, "/api/1/envelope/": http.StatusOK} {
		// A page on app.example.com posting to the panel on hakobu.example.com.
		r := httptest.NewRequest("POST", "https://hakobu.example.com"+path, nil)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Origin", "https://app.example.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s from a sibling subdomain: %d, want %d", path, w.Code, want)
		}
		if w.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: no X-Frame-Options", path)
		}
	}
	r := httptest.NewRequest("POST", "https://hakobu.example.com/settings/access", nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin POST: %d", w.Code)
	}
}

func TestStaticFiles(t *testing.T) {
	page := renders(t, "login", loginPage(true, ""))
	if strings.Contains(page, "https://") {
		t.Error("the login page loads something from another site")
	}
	h := staticHandler()
	for _, name := range []string{"app.css", "app.js", "htmx.min.js"} {
		u := staticURL(name)
		if !strings.Contains(page, u) {
			t.Errorf("page doesn't link %s", u)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		if w.Code != http.StatusOK || w.Body.Len() == 0 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d, %d bytes, %q", u, w.Code, w.Body.Len(), w.Header().Get("Cache-Control"))
		}
	}
	// The stylesheet's fonts are embedded too.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/static/fonts/inter.woff2", nil))
	if w.Code != http.StatusOK {
		t.Errorf("font: %d", w.Code)
	}
}

func TestMasterKeyNeedsFreshSignIn(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := config.PrepareDataDir(); err != nil {
		t.Fatal(err)
	}
	if err := config.SetPublicHost("hakobu.example.com"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SetOwner(ctx, store.SetOwnerParams{GitHubID: 42, GitHubLogin: "me"}); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, "tok", 42, time.Hour); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://hakobu.example.com/settings/master-key", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
		w := httptest.NewRecorder()
		panelHandler(mux).ServeHTTP(w, r)
		return w
	}

	keyFreshFor = -time.Second // signed in too long ago
	t.Cleanup(func() { keyFreshFor = 5 * time.Minute })
	w := get()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/auth/login" || !strings.Contains(w.Header().Get("Set-Cookie"), afterCookie+"=master-key") {
		t.Errorf("stale session: %d %v, body %q", w.Code, w.Header(), w.Body)
	}
	if ops.KeyDownloaded() {
		t.Error("key counted as downloaded")
	}

	keyFreshFor = time.Hour
	w = get()
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || !strings.Contains(w.Body.String(), "HAKOBU_MASTER_KEY=") {
		t.Errorf("fresh session: %d %v, body %q", w.Code, w.Header(), w.Body)
	}
	if !ops.KeyDownloaded() {
		t.Error("download not noted")
	}
}

func TestAppCalls(t *testing.T) {
	apps := []store.App{
		{Name: "web", Domain: "acme.example.com", Env: "VITE_API_URL=https://api.acme.example.com\nSITE=acme.example.com"},
		{Name: "api", Domain: "api.acme.example.com", Env: "CORS_ORIGIN=https://acme.example.com/\nSEARCH=http://search.hakobu:8080"},
		{Name: "search", Env: "X=searchable.hakobu.io"},
		{Name: "admin", Env: "API=https://API.ACME.example.com"},
	}
	got := appCalls(apps)
	want := map[string][]string{"web": {"api"}, "api": {"web", "search"}, "admin": {"api"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("appCalls = %v, want %v", got, want)
	}
}

func TestLogos(t *testing.T) {
	for _, c := range []struct {
		app  store.App
		want string
	}{
		{store.App{Stack: "Python · FastAPI"}, "fastapi"},
		{store.App{Stack: "Node.js · React"}, "react"},
		{store.App{Stack: "Ruby · Rails"}, "rubyonrails"},
		{store.App{Stack: "Rust"}, ""}, // no CC0 logo
		{store.App{Stack: "Rust", BuildStrategy: "dockerfile"}, "docker"},
		{store.App{Stack: "Python"}, "python"},
	} {
		if got := appLogo(c.app); got != c.want {
			t.Errorf("appLogo(%q) = %q, want %q", c.app.Stack, got, c.want)
		}
	}
	if got := workerLogo(store.App{Stack: "Python · Django"}, "celery -A proj worker"); got != "celery" {
		t.Errorf("celery worker: %q", got)
	}
	if got := workerLogo(store.App{Stack: "Python · FastAPI"}, "taskiq worker app.tasks:broker"); got != "python" {
		t.Errorf("taskiq worker: %q", got)
	}
	// Every logo the mapping names is in the sprite.
	for _, slug := range append(slices.Collect(maps.Values(stackLogos)), "docker", "celery", "postgresql", "cloudflare") {
		renders(t, slug, logo(slug))
	}
}
