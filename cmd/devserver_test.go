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

	"github.com/x0ryz/hakobu/internal/config"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
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
	must(s.SetOwner(ctx, store.SetOwnerParams{GitHubID: 42, GitHubLogin: "x0ryz"}))
	must(s.NewSession(ctx, "dev", 42, 24*3600*1e9))

	for _, p := range []string{"acme", "shop"} {
		must(s.CreateProject(ctx, p))
	}
	acme, _ := s.GetProject(ctx, "acme")
	shop, _ := s.GetProject(ctx, "shop")
	app := func(p store.Project, name, repo, strategy, path, domain string) {
		must(s.CreateApp(ctx, store.CreateAppParams{ProjectID: p.ID, Name: name, Repo: repo, BuildStrategy: strategy, BuildPath: path}))
		if domain != "" {
			must(s.SetAppDomain(ctx, store.SetAppDomainParams{Name: name, Domain: domain}))
		}
	}
	app(acme, "web", "x0ryz/acme", "railpack", "web", "acme.example.com")
	app(acme, "api", "x0ryz/acme", "dockerfile", "api", "api.acme.example.com")
	app(acme, "admin", "x0ryz/acme-admin", "railpack", ".", "")
	app(shop, "store", "x0ryz/shop", "railpack", ".", "shop.example.com")
	must(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "main", ProjectID: acme.ID, User: "main"}))
	must(s.CreateDatabase(ctx, store.CreateDatabaseParams{Name: "analytics", ProjectID: acme.ID, User: "analytics"}))
	must(s.CreateStorage(ctx, store.CreateStorageParams{Name: "files", ProjectID: acme.ID, Provider: "r2", Bucket: "hakobu-files-1", AccessKeyID: "abc123"}))
	must(s.CreateStorage(ctx, store.CreateStorageParams{Name: "media", ProjectID: acme.ID, Provider: "s3", Bucket: "media-eu"}))
	for _, a := range []string{"api"} {
		must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{Name: a, LinkedDB: "main"}))
		must(s.SetAppLinkedStorage(ctx, store.SetAppLinkedStorageParams{Name: a, LinkedStorage: "files"}))
	}
	must(s.SetAppLinkedDB(ctx, store.SetAppLinkedDBParams{Name: "admin", LinkedDB: "analytics"}))
	must(s.SaveWorker(ctx, store.SaveWorkerParams{AppName: "api", Name: "worker", Command: "taskiq worker app.tasks:broker", Env: "CONCURRENCY=4"}))
	must(s.AddVolume(ctx, store.AddVolumeParams{AppName: "api", Name: "data", MountPath: "/app/data"}))
	must(s.SetAppEnv(ctx, store.SetAppEnvParams{Name: "web", Env: "VITE_API_URL=https://api.acme.example.com"}))
	for app, stack := range map[string]string{"web": "Node.js · React", "api": "Python · FastAPI", "admin": "Python · Django", "store": "Node.js · Next.js"} {
		must(s.SetAppStack(ctx, store.SetAppStackParams{Name: app, Stack: stack}))
	}
	for _, d := range []struct{ app, status, out string }{
		{"api", "failed", "#8 [build 3/5] RUN uv sync --frozen\nerror: lockfile needs to be updated"},
		{"api", "success", "#9 [build 5/5] done\nhealth check passed, live"},
		{"web", "success", "built in 31s"},
		{"admin", "success", "built in 12s"},
		{"store", "success", "built"},
	} {
		id, err := s.CreateDeployLog(ctx, store.CreateDeployLogParams{AppName: d.app, Trigger: "push", Status: "running"})
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
	far, _ := s.GetNodeByName(ctx, "globex-1")
	must(s.JoinNode(ctx, store.JoinNodeParams{PublicKey: "00", ID: far.ID}))
	must(s.SeeNode(ctx, store.SeeNodeParams{Version: "v0.7.0", LastSeen: "2026-10-03T10:00:00Z", ID: far.ID}))
	must(s.SetProjectNode(ctx, store.SetProjectNodeParams{NodeID: sql.NullInt64{Int64: far.ID, Valid: true}, ID: shop.ID}))
	vid, err := s.CreateVolumeBackup(ctx, store.CreateVolumeBackupParams{AppName: "api", Volume: "data", ObjectKey: "vol/api/data/1", SizeBytes: 42 << 20})
	must(err)
	must(s.SetVolumeBackupVerified(ctx, store.SetVolumeBackupVerifiedParams{ID: vid, VerifiedAt: "2026-10-03T03:00:00Z", Files: 1200}))
	aid, err := s.CreateBackup(ctx, store.CreateBackupParams{Database: "analytics", ObjectKey: "db/analytics/1", SizeBytes: 1 << 30})
	must(err)
	must(s.SetBackupVerified(ctx, store.SetBackupVerifiedParams{ID: aid, VerifiedAt: "2026-10-03T03:00:00Z", VerifyError: "pg_restore: out of disk space"}))
	id, err := s.CreateBackup(ctx, store.CreateBackupParams{Database: "main", ObjectKey: "db/main/1", SizeBytes: 860 << 20})
	must(err)
	must(s.SetBackupVerified(ctx, store.SetBackupVerifiedParams{ID: id, VerifiedAt: "2026-10-03T03:00:00Z", Tables: 24}))
	must(s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppName: "api", Kind: "error", Level: "error", Message: "TypeError: 'NoneType' object is not subscriptable\n  File \"app/billing.py\", line 88, in charge"}))
	must(s.Tel.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppName: "api", Kind: "oom", Message: "worker was killed for running out of memory (limit 256 MB)"}))

	mux := http.NewServeMux()
	registerWebRoutes(mux, s)
	h := panelHandler(mux)
	t.Logf("panel at http://%s", addr)
	must(http.ListenAndServe(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "dev"})
		h.ServeHTTP(w, r)
	})))
}
