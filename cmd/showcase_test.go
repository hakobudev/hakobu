package cmd

// The panel with everything healthy, for the README's screenshot:
//
//	HAKOBU_UI_SHOWCASE=127.0.0.1:8091 go test ./cmd -run TestShowcase -timeout 0
//
// A fake Docker socket says every container runs, usage is made up for the
// last day and the watchdog's record is answered here, without Cloudflare.
// TestDevServer is the one with the broken states.

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/ops"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

func TestShowcase(t *testing.T) {
	addr := os.Getenv("HAKOBU_UI_SHOWCASE")
	if addr == "" {
		t.Skip("set HAKOBU_UI_SHOWCASE=host:port")
	}
	t.Chdir(t.TempDir())
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(config.PrepareDataDir())
	must(config.SetPublicHost("hakobu.example.com"))
	s, err := store.Open(config.DatabaseFile)
	must(err)
	ctx := context.Background()
	me, err := s.CreateUser(ctx, store.CreateUserParams{GitHubID: 42, GitHubLogin: "x0ryz", Admin: 1})
	must(err)
	must(s.NewSession(ctx, "dev", 42, 24*3600*1e9))

	id := func(v int64, err error) int64 { must(err); return v }
	acmeID := id(s.CreateProject(ctx, store.CreateProjectParams{Name: "acme", UserID: me}))
	apps := map[string]int64{}
	app := func(name, repo, strategy, path, domain, stack string) {
		apps[name] = id(s.CreateApp(ctx, store.CreateAppParams{ProjectID: acmeID, Name: name, Repo: repo, BuildStrategy: strategy, BuildPath: path}))
		if domain != "" {
			must(s.SetAppDomain(ctx, store.SetAppDomainParams{ID: apps[name], Domain: domain}))
		}
		must(s.SetAppStack(ctx, store.SetAppStackParams{ID: apps[name], Stack: stack}))
	}
	app("web", "x0ryz/acme", "railpack", "web", "acme.example.com", "Node.js · React")
	app("api", "x0ryz/acme", "dockerfile", "api", "api.acme.example.com", "Python · FastAPI")
	app("admin", "x0ryz/acme-admin", "railpack", ".", "", "Python · Django")
	mainDB := id(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "main", ProjectID: acmeID}))
	analytics := id(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "analytics", ProjectID: acmeID}))
	files := id(s.CreateStorage(ctx, store.CreateStorageParams{Name: "files", ProjectID: acmeID, Provider: "r2", Bucket: "acme-files", AccessKeyID: "abc123"}))
	ref := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{ID: apps["api"], LinkedDbID: ref(mainDB)}))
	must(s.SetAppLinkedStorage(ctx, store.SetAppLinkedStorageParams{ID: apps["api"], LinkedStorageID: ref(files)}))
	must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{ID: apps["admin"], LinkedDbID: ref(analytics)}))
	must(s.SaveWorker(ctx, store.SaveWorkerParams{AppID: apps["api"], Name: "worker", Command: "taskiq worker app.tasks:broker", Env: "CONCURRENCY=4"}))
	must(s.AddVolume(ctx, store.AddVolumeParams{AppID: apps["api"], Name: "data", MountPath: "/app/data"}))
	must(s.SetAppRoute(ctx, store.SetAppRouteParams{AppID: apps["web"], Path: "/api", TargetID: apps["api"]}))
	cache := id(s.CreateCache(ctx, store.CreateCacheParams{Name: "cache", ProjectID: acmeID, Password: "dev", MemoryMB: 256}))
	must(s.SetAppLinkedCache(ctx, store.SetAppLinkedCacheParams{ID: apps["api"], LinkedCacheID: ref(cache)}))
	for _, d := range []struct{ app, out string }{
		{"api", "built in 48s"}, {"api", "built in 44s"}, {"api", "#9 [build 5/5] done\nhealth check passed, live"},
		{"web", "built in 31s"}, {"admin", "built in 12s"},
	} {
		id, err := s.CreateDeployLog(ctx, store.CreateDeployLogParams{AppID: apps[d.app], Trigger: "push", Status: "running"})
		must(err)
		must(s.UpdateDeployLog(ctx, store.UpdateDeployLogParams{ID: id, Status: "success", Output: secret.String(d.out)}))
	}
	must(s.SaveCloudflareToken(ctx, "dev"))
	must(s.SetBackupBucket(ctx, "hakobu-backups-1a2b3c"))
	vid, err := s.CreateVolumeBackup(ctx, store.CreateVolumeBackupParams{AppID: apps["api"], Volume: "data", ObjectKey: "vol/api/data/1", SizeBytes: 42 << 20})
	must(err)
	must(s.SetVolumeBackupVerified(ctx, store.SetVolumeBackupVerifiedParams{ID: vid, VerifiedAt: "2026-10-03T03:00:00Z", Files: 1200}))
	for db, tables := range map[int64]int64{mainDB: 24, analytics: 9} {
		bid, err := s.CreateBackup(ctx, store.CreateBackupParams{DatabaseID: db, ObjectKey: "db/1", SizeBytes: 860 << 20})
		must(err)
		must(s.SetBackupVerified(ctx, store.SetBackupVerifiedParams{ID: bid, VerifiedAt: "2026-10-03T03:00:00Z", Tables: tables}))
	}
	must(s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: apps["api"], Kind: "error", Level: "error", Message: "KeyError: 'body'\n  File \"chat/stream.py\", line 42, in reply", Fingerprint: "reply"}))

	// A day of usage, minute by minute: a daily wave with some noise.
	now := time.Now().Truncate(time.Minute)
	for name, base := range map[string]float64{"web": 0.08, "api": 0.22, "admin": 0.03} {
		for m := 0; m < 24*60; m++ {
			ts := now.Add(-time.Duration(m) * time.Minute).Unix()
			x := float64(ts)
			wave := 1 + 0.6*math.Sin(x/86400*2*math.Pi) + 0.25*math.Sin(x/1700) + 0.15*math.Sin(x/310)
			cpu := base * wave
			mem := int64((180 + 40*wave) * (1 << 20) * (base*4 + 0.5))
			must(s.Tel.PutSample(ctx, teldb.PutSampleParams{
				Target: ops.AppTarget(apps[name]), Res: 60, Slot: ts / 60 % (48 * 60), Ts: ts,
				Cpu: cpu, CpuMax: cpu * 1.3, Mem: mem, MemMax: mem + 8<<20, MemLimit: 512 << 20,
				NetRx: 40e3 * wave, NetTx: 120e3 * wave,
			}))
		}
	}

	// Docker: every container runs.
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	must(err)
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	var names []string
	for _, a := range apps {
		names = append(names, "/"+store.AppKey(a)+"-blue", "/"+store.AppKey(a)+"-worker")
	}
	names = append(names, "/cache-"+itoa(cache))
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, path, _ := strings.Cut(r.URL.Path, "/containers")
		path = "/containers" + path
		switch {
		case path == "/containers/json":
			json.NewEncoder(w).Encode([]map[string]any{{"Names": names, "State": "running"}})
		case strings.HasSuffix(path, "/json"):
			json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Status": "running"}})
		default:
			http.NotFound(w, r)
		}
	}))

	// The watchdog's record: a short outage two weeks ago.
	up := ops.Uptime{AvgMs: 84, Last: now}
	for i, label := range []string{"24 hours", "7 days", "30 days"} {
		up.Windows = append(up.Windows, ops.UptimeWindow{Label: label, Pct: []float64{100, 100, 99.95}[i], Checks: []int64{1440, 10080, 43200}[i]})
	}
	for d := 29; d >= 0; d-- {
		pct := 100.0
		if d == 14 {
			pct = 98.6
		}
		up.Days = append(up.Days, ops.UptimeDay{Day: now.Truncate(24*time.Hour).AddDate(0, 0, -d), Pct: pct, Checks: 1440})
	}
	start := now.Truncate(24*time.Hour).AddDate(0, 0, -14).Add(3*time.Hour + 12*time.Minute)
	up.Outages = []ops.Outage{{Start: start, End: start.Add(19 * time.Minute), Status: 502}}

	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	h := panelHandler(mux)
	t.Logf("panel at http://%s", addr)
	must(http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "dev"})
		if r.URL.Path == "/uptime" {
			v := uptimeView{Uptime: up}
			if r.URL.Query().Get("compact") != "" {
				renderPage(w, r, uptimeStrip(v))
				return
			}
			renderPage(w, r, uptimeFragment(v))
			return
		}
		h.ServeHTTP(w, r)
	})))
}
