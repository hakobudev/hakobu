package cmd

// A local panel with made-up data, to look at the pages in a browser:
//
//	HAKOBU_UI_DEV=127.0.0.1:8090 go test ./cmd -run TestDevServer -timeout 0
//
// It signs every request in as the owner. Not for anything but looking.

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"testing"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
	"github.com/hakobudev/hakobu/internal/store/teldb"
)

func TestDevServer(t *testing.T) {
	addr := os.Getenv("HAKOBU_UI_DEV")
	if addr == "" {
		t.Skip("set HAKOBU_UI_DEV=host:port")
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
	shopID := id(s.CreateProject(ctx, store.CreateProjectParams{Name: "shop", UserID: me}))
	shop, _ := s.GetProjectByID(ctx, shopID)
	apps := map[string]int64{}
	app := func(p int64, name, repo, strategy, path, domain string) {
		apps[name] = id(s.CreateApp(ctx, store.CreateAppParams{ProjectID: p, Name: name, Repo: repo, BuildStrategy: strategy, BuildPath: path}))
		if domain != "" {
			must(s.SetAppDomain(ctx, store.SetAppDomainParams{ID: apps[name], Domain: domain}))
		}
	}
	app(acmeID, "web", "x0ryz/acme", "railpack", "web", "acme.example.com")
	app(acmeID, "api", "x0ryz/acme", "dockerfile", "api", "api.acme.example.com")
	app(acmeID, "admin", "x0ryz/acme-admin", "railpack", ".", "")
	app(shopID, "store", "x0ryz/shop", "railpack", ".", "shop.example.com")
	mainDB := id(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "main", ProjectID: acmeID}))
	analytics := id(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "analytics", ProjectID: acmeID}))
	files := id(s.CreateStorage(ctx, store.CreateStorageParams{Name: "files", ProjectID: acmeID, Provider: "r2", Bucket: "hakobu-files-1", AccessKeyID: "abc123"}))
	id(s.CreateStorage(ctx, store.CreateStorageParams{Name: "media", ProjectID: acmeID, Provider: "s3", Bucket: "media-eu"}))
	ref := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{ID: apps["api"], LinkedDbID: ref(mainDB)}))
	must(s.SetAppLinkedStorage(ctx, store.SetAppLinkedStorageParams{ID: apps["api"], LinkedStorageID: ref(files)}))
	must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{ID: apps["admin"], LinkedDbID: ref(analytics)}))
	must(s.SaveWorker(ctx, store.SaveWorkerParams{AppID: apps["api"], Name: "worker", Command: "taskiq worker app.tasks:broker", Env: "CONCURRENCY=4"}))
	must(s.AddVolume(ctx, store.AddVolumeParams{AppID: apps["api"], Name: "data", MountPath: "/app/data"}))
	must(s.SetAppRoute(ctx, store.SetAppRouteParams{AppID: apps["web"], Path: "/api", TargetID: apps["api"]}))
	cache := id(s.CreateCache(ctx, store.CreateCacheParams{Name: "cache", ProjectID: acmeID, Password: "dev", MemoryMB: 256}))
	must(s.SetAppLinkedCache(ctx, store.SetAppLinkedCacheParams{ID: apps["api"], LinkedCacheID: ref(cache)}))
	for _, l := range []struct {
		level int64
		msg   string
	}{
		{0, "server node-1 connected, hakobu v0.20.0"},
		{1, "tunnel routes not updated (retrying in 5s): context deadline exceeded"},
		{2, "backup failed for main: R2 is off in the panel's Cloudflare account"},
	} {
		must(s.Tel.AddPanelLog(ctx, teldb.AddPanelLogParams{Level: l.level, Message: secret.String(l.msg)}))
	}
	must(s.SetAppEnv(ctx, store.SetAppEnvParams{ID: apps["web"], Env: "VITE_API_URL=https://api.acme.example.com"}))
	for app, stack := range map[string]string{"web": "Node.js · React", "api": "Python · FastAPI", "admin": "Python · Django", "store": "Node.js · Next.js"} {
		must(s.SetAppStack(ctx, store.SetAppStackParams{ID: apps[app], Stack: stack}))
	}
	for _, d := range []struct{ app, status, out string }{
		{"api", "failed", "#8 [build 3/5] RUN uv sync --frozen\nerror: lockfile needs to be updated"},
		{"api", "success", "#9 [build 5/5] done\nhealth check passed, live"},
		{"web", "success", "built in 31s"},
		{"admin", "success", "built in 12s"},
		{"store", "success", "built"},
	} {
		id, err := s.CreateDeployLog(ctx, store.CreateDeployLogParams{AppID: apps[d.app], Trigger: "push", Status: "running"})
		must(err)
		must(s.UpdateDeployLog(ctx, store.UpdateDeployLogParams{ID: id, Status: d.status, Output: secret.String(d.out)}))
	}
	// Backups on, as if Cloudflare were connected; nothing here calls it.
	must(s.SaveCloudflareToken(ctx, "dev"))
	must(s.SetBackupBucket(ctx, "hakobu-backups-1a2b3c"))
	client, err := s.CreateCloudflareAccount(ctx, store.CreateCloudflareAccountParams{Name: "globex", ApiToken: "dev", AccountID: "9f3c2a1b"})
	must(err)
	must(s.SetProjectCloudflareAccount(ctx, store.SetProjectCloudflareAccountParams{CloudflareAccountID: sql.NullInt64{Int64: client, Valid: true}, ID: shop.ID}))
	must(s.CreateNode(ctx, store.CreateNodeParams{Name: "globex-1"}))
	must(s.CreateNode(ctx, store.CreateNodeParams{Name: "spare"}))
	far, _ := s.GetNodeByName(ctx, store.GetNodeByNameParams{Name: "globex-1"})
	must(s.JoinNode(ctx, store.JoinNodeParams{PublicKey: "00", ID: far.ID}))
	must(s.SeeNode(ctx, store.SeeNodeParams{Version: "v0.7.0", LastSeen: "2026-10-03T10:00:00Z", ID: far.ID}))
	must(s.SetProjectNode(ctx, store.SetProjectNodeParams{NodeID: sql.NullInt64{Int64: far.ID, Valid: true}, ID: shop.ID}))
	vid, err := s.CreateVolumeBackup(ctx, store.CreateVolumeBackupParams{AppID: apps["api"], Volume: "data", ObjectKey: "vol/api/data/1", SizeBytes: 42 << 20})
	must(err)
	must(s.SetVolumeBackupVerified(ctx, store.SetVolumeBackupVerifiedParams{ID: vid, VerifiedAt: "2026-10-03T03:00:00Z", Files: 1200}))
	aid, err := s.CreateBackup(ctx, store.CreateBackupParams{DatabaseID: analytics, ObjectKey: "db/analytics/1", SizeBytes: 1 << 30})
	must(err)
	must(s.SetBackupVerified(ctx, store.SetBackupVerifiedParams{ID: aid, VerifiedAt: "2026-10-03T03:00:00Z", VerifyError: "pg_restore: out of disk space"}))
	bid, err := s.CreateBackup(ctx, store.CreateBackupParams{DatabaseID: mainDB, ObjectKey: "db/main/1", SizeBytes: 860 << 20})
	must(err)
	must(s.SetBackupVerified(ctx, store.SetBackupVerifiedParams{ID: bid, VerifiedAt: "2026-10-03T03:00:00Z", Tables: 24}))
	must(s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: apps["api"], Kind: "error", Level: "error", Message: "TypeError: 'NoneType' object is not subscriptable\n  File \"app/billing.py\", line 88, in charge"}))
	must(s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: apps["api"], Kind: "oom", Message: "worker was killed for running out of memory (limit 256 MB)"}))

	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	h := panelHandler(mux)
	t.Logf("panel at http://%s", addr)
	must(http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "dev"})
		h.ServeHTTP(w, r)
	})))
}
