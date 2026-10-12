// Package ops holds hakobu's operations on projects, apps, databases and
// storages, shared by the dashboard, the webhook and the agent loops.
package ops

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hakobudev/hakobu/internal/config"
	"github.com/hakobudev/hakobu/internal/detect"
	"github.com/hakobudev/hakobu/internal/github"
	"github.com/hakobudev/hakobu/internal/panellog"
	"github.com/hakobudev/hakobu/internal/secret"
	"github.com/hakobudev/hakobu/internal/store"
)

func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Names are what people see and type, and go in URLs and bucket names;
// containers, Postgres and the rest go by ID (store.AppKey).
var (
	validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)
	// Database names are always double-quoted in SQL, where dashes are fine.
	validDBName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

func checkName(kind, name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid %s name %q: use lowercase letters, digits and dashes, starting with a letter", kind, name)
	}
	return nil
}

// Projects.

func CreateProject(s *store.Store, userID int64, name string) (int64, error) {
	return CreateProjectOn(s, userID, name, "", "")
}

// errPanelOnly is what a panel-only panel says to a project that would use
// its own server or Cloudflare account.
var errPanelOnly = errors.New("this panel runs no apps of its own: a project runs on a server of yours (Settings → Servers), in a Cloudflare account of yours (Settings → Cloudflare)")

// CreateProjectOn creates a project of user's on a server of theirs (""
// for the panel's), in a Cloudflare account of theirs ("" for the panel's).
func CreateProjectOn(s *store.Store, userID int64, name, serverName, client string) (int64, error) {
	if err := checkName("project", name); err != nil {
		return 0, err
	}
	if config.PanelOnly && (serverName == "" || client == "") {
		return 0, errPanelOnly
	}
	var account sql.NullInt64
	if client != "" {
		a, err := s.GetCloudflareAccountByName(ctx(), client)
		if err != nil || a.UserID != userID {
			return 0, fmt.Errorf("Cloudflare account %q not found", client)
		}
		account = sql.NullInt64{Int64: a.ID, Valid: true}
	}
	// Its networks are created when it first needs them.
	id, err := s.CreateProject(ctx(), store.CreateProjectParams{Name: name, UserID: userID})
	if err != nil {
		return 0, taken(err, "a project named %q", name)
	}
	undo := func(err error) (int64, error) {
		_ = s.DeleteProject(ctx(), id)
		return 0, err
	}
	if serverName != "" {
		if err := SetProjectServer(s, id, serverName); err != nil {
			return undo(err)
		}
	}
	if account.Valid {
		if err := s.SetProjectCloudflareAccount(ctx(), store.SetProjectCloudflareAccountParams{CloudflareAccountID: account, ID: id}); err != nil {
			return undo(err)
		}
	}
	return id, nil
}

// taken says that what's named exists already, for a UNIQUE constraint
// that failed.
func taken(err error, what string, args ...any) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf(what+" already exists", args...)
	}
	return err
}

// DeleteProject removes every app, database and storage in the project.
func DeleteProject(s *store.Store, id int64) error {
	p, err := s.GetProjectByID(ctx(), id)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}
	apps, err := s.ListAppsByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, a := range apps {
		if err := DeleteApp(s, a.ID); err != nil {
			return err
		}
	}
	dbs, err := s.ListDatabasesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if err := DeleteDatabase(s, d.ID); err != nil {
			return err
		}
	}
	storages, err := s.ListStoragesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, st := range storages {
		if err := DeleteStorage(s, st.ID); err != nil {
			return err
		}
	}
	caches, err := s.ListCachesByProject(ctx(), p.ID)
	if err != nil {
		return err
	}
	for _, c := range caches {
		if err := DeleteCache(s, c.ID); err != nil {
			return err
		}
	}
	if err := s.DeleteSealedVarsOf(ctx(), store.DeleteSealedVarsOfParams{Scope: "project", OwnerID: p.ID}); err != nil {
		return err
	}
	if err := removeProjectNetworks(s, p); err != nil {
		return err
	}
	return s.DeleteProject(ctx(), p.ID)
}

// Apps.

// CreateApp stores the app in its project (app.ProjectID), gives it its
// domain ("" keeps it private) and kicks off its first build and deploy.
func CreateApp(s *store.Store, app store.CreateAppParams, domain string) (int64, error) {
	return CreateAppWith(s, app, domain, "")
}

// CreateAppWith is CreateApp for an app with a start command of its own,
// set before its first deploy.
func CreateAppWith(s *store.Store, app store.CreateAppParams, domain, startCommand string) (int64, error) {
	if err := checkName("app", app.Name); err != nil {
		return 0, err
	}
	if app.BuildStrategy == "" {
		app.BuildStrategy = "railpack"
	}
	var err error
	if app.BuildPath, app.BuildStrategy, err = checkBuild(app.BuildPath, app.BuildStrategy); err != nil {
		return 0, err
	}
	if _, err := s.GetProjectByID(ctx(), app.ProjectID); err != nil {
		return 0, fmt.Errorf("project not found: %w", err)
	}
	if err := CheckDomain(s, 0, domain); err != nil {
		return 0, err
	}
	id, err := s.CreateApp(ctx(), app)
	if err != nil {
		return 0, taken(err, "an app named %q", app.Name)
	}
	created, err := s.GetAppByID(ctx(), id)
	if err != nil {
		return 0, err
	}
	if err := SetAppDomain(s, created, domain); err != nil {
		_ = s.DeleteAppCascade(ctx(), id)
		return 0, err
	}
	if startCommand = strings.TrimSpace(startCommand); startCommand != "" {
		if err := s.SetAppBuild(ctx(), store.SetAppBuildParams{ID: id, BuildPath: app.BuildPath, BuildStrategy: app.BuildStrategy, StartCommand: startCommand}); err != nil {
			_ = s.DeleteAppCascade(ctx(), id)
			return 0, err
		}
	}
	async(func() { watchApps(s) })
	return id, StartDeploy(s, id, "create")
}

// watchApps has the watchdogs check the apps there are now.
func watchApps(s *store.Store) {
	if err := EnsureWatchdog(s, false); err != nil {
		panellog.Error("watchdog:", err)
	}
	EnsureAccountAlerts(s)
}

// SetHealthCheck sets the path an app's health is checked at: "" for any
// answer but a server error at /, a path for a 2xx there. Deploys check it
// before switching traffic, and the watchdog from outside, from now on.
func SetHealthCheck(s *store.Store, app store.App, path string) error {
	path = strings.TrimSpace(path)
	if path == "/" {
		path = ""
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if strings.ContainsAny(path, " \t\n?#") || len(path) > 200 {
		return fmt.Errorf("%q isn't a path: give one like /health, without a query", path)
	}
	if err := s.SetAppSettings(ctx(), store.SetAppSettingsParams{ID: app.ID, ContainerPort: app.ContainerPort, HealthCheckPath: path}); err != nil {
		return err
	}
	if path != app.HealthCheckPath {
		async(func() { watchApps(s) })
	}
	return nil
}

// SetAppBuild changes how the app is built (strategy "railpack" or
// "dockerfile", path of the build context inside the repo) and started (a
// shell command instead of the image's own start, "" for that). Applies on
// the next deploy; no need to delete and recreate the app.
func SetAppBuild(s *store.Store, appID int64, path, strategy, startCommand string) error {
	path, strategy, err := checkBuild(path, strategy)
	if err != nil {
		return err
	}
	startCommand = strings.TrimSpace(startCommand)
	return s.SetAppBuild(ctx(), store.SetAppBuildParams{ID: appID, BuildPath: path, BuildStrategy: strategy, StartCommand: startCommand})
}

// checkBuild cleans up a build path in the repo and a strategy, and
// rejects what can't be built.
func checkBuild(path, strategy string) (string, string, error) {
	strategy = strings.TrimSpace(strategy)
	if strategy != "railpack" && strategy != "dockerfile" {
		return "", "", fmt.Errorf("unknown build strategy %q: railpack or dockerfile", strategy)
	}
	path = strings.Trim(strings.TrimSpace(path), "/.")
	if strings.Contains(path, "..") {
		return "", "", fmt.Errorf("invalid build path %q", path)
	}
	return path, strategy, nil
}

// CheckDomain rejects a domain that the panel or another app than appID
// already uses; it doesn't say whose, as it may be another user's.
func CheckDomain(s *store.Store, appID int64, domain string) error {
	if domain == "" {
		return nil
	}
	if strings.EqualFold(domain, config.PublicHost()) {
		return fmt.Errorf("%s is the panel's own address", domain)
	}
	if other, err := s.GetAppByDomain(ctx(), domain); err == nil && other.ID != appID {
		return fmt.Errorf("%s is already in use", domain)
	}
	return nil
}

func DeleteApp(s *store.Store, id int64) error {
	// Hold the app like a deploy so no deploy recreates its container or
	// proxy while it's being removed.
	if _, err := reserve(id, false); err != nil {
		return err
	}
	defer release(id)
	app, err := s.GetAppByID(ctx(), id)
	if err != nil {
		return fmt.Errorf("app not found: %w", err)
	}
	vols, err := s.ListVolumes(ctx(), id)
	if err != nil {
		return err
	}
	if err := removeAppDNS(s, app); err != nil {
		panellog.Error("failed to delete the DNS record of", app.Domain+":", err)
	}
	// Its telemetry on another server goes with it; the panel's own goes
	// with its records.
	if t, err := appTelemetry(s, app); err == nil {
		if err := t.DropTelemetry(ctx(), id); err != nil {
			panellog.Warn("the telemetry of", app.Name, "stays on its server:", err)
		}
	}
	if err := s.DeleteAppCascade(ctx(), id); err != nil {
		return err
	}
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = v.Name
	}
	if err := AppNode(s, app).RemoveApp(ctx(), app.Key(), names); err != nil {
		return err
	}
	if err := SyncTunnel(s); err != nil {
		panellog.Warn("tunnel routes not updated (retrying):", err)
	}
	async(func() { watchApps(s) })
	return nil
}

// LinkDatabase links (or with dbName "" unlinks) one of the project's
// databases, by its name there.
func LinkDatabase(s *store.Store, app store.App, dbName string) error {
	var id sql.NullInt64
	if dbName != "" {
		d, err := s.GetDatabase(ctx(), store.GetDatabaseParams{ProjectID: app.ProjectID, Name: dbName})
		if err != nil {
			return fmt.Errorf("database %q not found in project %s", dbName, app.ProjectName)
		}
		id = sql.NullInt64{Int64: d.ID, Valid: true}
	}
	return s.SetAppLinkedDB(ctx(), store.SetAppLinkedDBParams{ID: app.ID, LinkedDbID: id})
}

// LinkStorage links (or with storageName "" unlinks) one of the project's
// storages, by its name there.
func LinkStorage(s *store.Store, app store.App, storageName string) error {
	var id sql.NullInt64
	if storageName != "" {
		st, err := s.GetStorage(ctx(), store.GetStorageParams{ProjectID: app.ProjectID, Name: storageName})
		if err != nil {
			return fmt.Errorf("storage %q not found in project %s", storageName, app.ProjectName)
		}
		if st.AccessKeyID == "" {
			return fmt.Errorf("storage %s has no keys yet: add them on the project page", storageName)
		}
		id = sql.NullInt64{Int64: st.ID, Valid: true}
	}
	return s.SetAppLinkedStorage(ctx(), store.SetAppLinkedStorageParams{ID: app.ID, LinkedStorageID: id})
}

// Environment. Later entries win on duplicate keys, so the order is:
// PORT, linked database, storage and cache, SENTRY_DSN, project shared
// variables (visible, then sealed), then the service's own (likewise).

// baseEnv starts with PORT unless port is 0 (workers).
func baseEnv(s *store.Store, app store.App, port int64) ([]string, error) {
	var env []string
	if port > 0 {
		env = append(env, fmt.Sprintf("PORT=%d", port))
	}
	if app.LinkedDbID.Valid {
		d, err := s.GetDatabaseByID(ctx(), app.LinkedDbID.Int64)
		if err != nil {
			return nil, fmt.Errorf("linked database not found: %w", err)
		}
		env = append(env, DatabaseEnv(d)...)
	}
	if app.LinkedStorageID.Valid {
		st, err := s.GetStorageByID(ctx(), app.LinkedStorageID.Int64)
		if err != nil {
			return nil, fmt.Errorf("linked storage not found: %w", err)
		}
		env = append(env, storageEnv(st)...)
	}
	if app.LinkedCacheID.Valid {
		c, err := s.GetCacheByID(ctx(), app.LinkedCacheID.Int64)
		if err != nil {
			return nil, fmt.Errorf("linked cache not found: %w", err)
		}
		env = append(env, CacheEnv(c)...)
	}
	if host := config.PublicHost(); host != "" {
		key := string(app.SentryKey)
		if key == "" {
			var err error
			if key, err = RandomHex(16); err != nil {
				return nil, err
			}
			if err := s.SetAppSentryKey(ctx(), store.SetAppSentryKeyParams{ID: app.ID, SentryKey: secret.String(key)}); err != nil {
				return nil, err
			}
		}
		// Server SDKs report inside the server: from its address
		// Cloudflare may challenge them. Browser SDKs need the public one.
		env = append(env, "SENTRY_DSN="+ingestDSN(key, app.ID), fmt.Sprintf("SENTRY_PUBLIC_DSN=https://%s@%s/%d", key, host, app.ID))
	}
	p, err := s.GetProjectByID(ctx(), app.ProjectID)
	if err != nil {
		return nil, err
	}
	return withScope(s, append(env, ParseEnv(string(p.SharedEnv))...), "project", p.ID)
}

// withScope adds a scope's sealed variables on top of env.
func withScope(s *store.Store, env []string, scope string, owner int64) ([]string, error) {
	sealed, err := sealedEnv(s, scope, owner)
	if err != nil {
		return nil, err
	}
	return append(env, sealed...), nil
}

func appEnv(s *store.Store, app store.App, port int64) ([]string, error) {
	env, err := baseEnv(s, app, port)
	if err != nil {
		return nil, err
	}
	if env, err = withScope(s, append(env, ParseEnv(string(app.Env))...), "app", app.ID); err != nil {
		return nil, err
	}
	return expandRefs(s, app, env)
}

func WorkerEnv(s *store.Store, app store.App, w store.Worker) ([]string, error) {
	env, err := baseEnv(s, app, 0)
	if err != nil {
		return nil, err
	}
	if env, err = withScope(s, append(env, ParseEnv(string(w.Env))...), "worker", app.ID); err != nil {
		return nil, err
	}
	return expandRefs(s, app, env)
}

// ParseEnv turns "KEY=value" lines into a docker env slice, skipping blank
// lines and comments.
func ParseEnv(text string) []string {
	var env []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		if line == "" || strings.HasPrefix(line, "#") || strings.Index(line, "=") <= 0 {
			continue
		}
		env = append(env, line)
	}
	return env
}

// GitHub.

func gitHubApp(s *store.Store) (store.GitHubApp, error) {
	app, err := s.GetGitHubApp(ctx())
	if err != nil {
		return app, fmt.Errorf("no GitHub App connected")
	}
	return app, nil
}

func repoToken(s *store.Store, repo string) (string, error) {
	app, err := gitHubApp(s)
	if err != nil {
		return "", err
	}
	return github.RepoToken(app.AppID, string(app.PrivateKey), repo)
}

// ListRepos lists the repositories a user may deploy: those of the GitHub
// App's installation on their own GitHub account.
func ListRepos(s *store.Store, githubID int64) ([]string, error) {
	app, err := gitHubApp(s)
	if err != nil {
		return nil, err
	}
	repos, err := github.ListRepos(app.AppID, string(app.PrivateKey), githubID)
	if err != nil {
		return nil, err
	}
	sort.Strings(repos)
	return repos, nil
}

// CheckRepo checks that a user may deploy repo.
func CheckRepo(s *store.Store, githubID int64, repo string) error {
	repos, err := ListRepos(s, githubID)
	if err != nil {
		return err
	}
	if !slices.Contains(repos, repo) {
		return fmt.Errorf("repository %q isn't among yours: install the GitHub App on your account and give it access", repo)
	}
	return nil
}

// ScanRepoPresets detects buildable directories of a user's repository
// through the GitHub API, without cloning.
func ScanRepoPresets(s *store.Store, githubID int64, repo string) ([]detect.Preset, error) {
	if err := CheckRepo(s, githubID, repo); err != nil {
		return nil, err
	}
	token, err := repoToken(s, repo)
	if err != nil {
		return nil, err
	}
	files, err := github.RepoFiles(repo, token)
	if err != nil {
		return nil, err
	}
	presets := detect.Scan(files, func(path string) string {
		content, _ := github.FileContent(repo, path, token)
		return content
	})
	if len(presets) == 0 {
		return nil, fmt.Errorf("no Dockerfile or supported project files found in %s", repo)
	}
	return presets, nil
}

var envExampleFiles = []string{".env.example", ".env.sample", ".env.dist", ".env.template", "env.example"}

// FindEnvExampleKeys returns the variable names from the first
// .env.example-style file in the app's build path or the repo root.
func FindEnvExampleKeys(s *store.Store, app store.App) (file string, keys []string) {
	if app.Repo == "" {
		return "", nil
	}
	token, err := repoToken(s, app.Repo)
	if err != nil {
		return "", nil
	}
	dirs := []string{""}
	if p := strings.Trim(app.BuildPath, "/."); p != "" {
		dirs = []string{p + "/", ""}
	}
	for _, dir := range dirs {
		for _, name := range envExampleFiles {
			content, ok := github.FileContent(app.Repo, dir+name, token)
			if !ok {
				continue
			}
			for _, line := range ParseEnv(content) {
				key, _, _ := strings.Cut(line, "=")
				keys = append(keys, strings.TrimSpace(key))
			}
			return dir + name, keys
		}
	}
	return "", nil
}
