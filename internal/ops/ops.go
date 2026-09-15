// Package ops holds the project/database operations shared by the web dashboard and
// the agent's HTTP API, so both surfaces run exactly the same logic.
package ops

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"hako/internal/build"
	"hako/internal/config"
	"hako/internal/deploy"
	"hako/internal/detect"
	"hako/internal/github"
	"hako/internal/hostinfo"
	"hako/internal/proxy"
	"hako/internal/store"
)

// EnsureProjectProxy makes sure a project's traffic proxy is running and,
// if it was just created (e.g. right after an agent restart, when the
// in-memory proxy registry starts empty), seeds it with whatever container
// is currently marked active so traffic keeps flowing without waiting for
// the next deploy.
func EnsureProjectProxy(project store.Project) {
	isNew, err := proxy.Ensure(project.Name, project.Port)
	if err != nil || !isNew {
		return
	}
	ip, err := deploy.ContainerIP(context.Background(), project.ContainerName())
	if err != nil {
		return
	}
	target, err := url.Parse(fmt.Sprintf("http://%s:%d", ip, project.ContainerPort))
	if err != nil {
		return
	}
	proxy.SetTarget(project.Name, target)
}

// healthCheckPath is the path a project's health checks hit — configurable
// per project (SetProjectHealthCheckPath) since not every app answers on
// "/" (e.g. a bare API with a dedicated /healthz), falling back to "/" for
// projects that never set one and normalizing a missing leading slash so a
// user typing "healthz" doesn't produce a malformed URL.
func healthCheckPath(project store.Project) string {
	path := project.HealthCheckPath
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// healthCheckRequiresOK reports whether a non-2xx response should count as
// down. A project that never configured its own health check path is still
// being probed on a bare "/" it may never have handled, so any response at
// all has to count as alive; a project that explicitly set its own path
// presumably built a real health endpoint and its status code means
// something — see TimedHTTPCheck.
func healthCheckRequiresOK(project store.Project) bool {
	return project.HealthCheckPath != "" && project.HealthCheckPath != "/"
}

// CheckProjectHealth runs a live HTTP check against the project's host port
// and records the result, so the dashboard's uptime bar has real history
// instead of only the state at page-load time.
func CheckProjectHealth(s *store.Store, project store.Project) (up bool, responseMs int) {
	url := fmt.Sprintf("http://localhost:%d%s", project.Port, healthCheckPath(project))
	up, responseMs = deploy.TimedHTTPCheck(url, healthCheckRequiresOK(project))
	s.RecordHealthCheck("project:"+project.Name, up, responseMs)
	return up, responseMs
}

// CheckDatabaseHealth runs pg_isready against the database container and
// records the result, same purpose as CheckProjectHealth but for databases.
func CheckDatabaseHealth(s *store.Store, db store.Database) (up bool) {
	up, _ = deploy.PostgresReady(context.Background(), db.ContainerName)
	s.RecordHealthCheck("database:"+db.Name, up, 0)
	return up
}

// SampleResources records a CPU/memory data point for target (only if the
// container is actually running), feeding the dashboard's sparklines.
func SampleResources(s *store.Store, target, containerName string) {
	status, _, err := deploy.ContainerStatus(context.Background(), containerName)
	if err != nil || status != "running" {
		return
	}
	stats, err := deploy.GetContainerStats(context.Background(), containerName)
	if err != nil {
		return
	}
	s.RecordResourceSample(target, stats.CPUPercent, stats.MemUsedMB)
}

// SampleHostAndAgent records CPU/memory trend points for the machine
// hako runs on and for the agent process itself, so the "Server" tab
// can show real history, not just a snapshot at page-load time.
func SampleHostAndAgent(s *store.Store) {
	if host, err := hostinfo.Get("data"); err == nil {
		s.RecordResourceSample("host", host.CPUPercent, host.MemUsedMB)
	}

	cpu, _ := hostinfo.SelfCPUPercent()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	const mb = 1024.0 * 1024.0
	s.RecordResourceSample("agent", cpu, float64(mem.Sys)/mb)
}

func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func RedeployProject(s *store.Store, name string) (string, error) {
	project, err := s.GetProjectByName(name)
	if err != nil {
		return "", fmt.Errorf("project %q not found: %w", name, err)
	}

	imageTag := ImageTag(*project)

	env, err := AppEnv(s, *project)
	if err != nil {
		logDeploy(s, name, "manual", "failed", err.Error())
		return "", err
	}

	var out strings.Builder
	containerID, err := DeployImage(s, *project, imageTag, env, &out)
	if err != nil {
		logDeploy(s, name, "manual", "failed", out.String())
		return "", err
	}
	logDeploy(s, name, "manual", "success", out.String())
	return containerID, nil
}

// ProjectSentryDSN returns the DSN a deployed project's Sentry SDK should
// use to report errors/logs straight to this hako instance, in the
// standard "https://<public_key>@<host>/<project_id>" form every Sentry SDK
// already knows how to parse — the project's own row id doubles as the
// path's project_id (see store.GetProjectBySentryProjectID), so there's
// nothing extra to track. ingestBaseURL is the externally reachable host a
// deployed container can use to reach the agent (e.g. the public host from Settings → Access);
// an empty ingestBaseURL means hako isn't reachable from deployed
// containers yet, so DSN injection is skipped rather than shipping a dead
// one.
func ProjectSentryDSN(s *store.Store, project store.Project, ingestBaseURL string) (string, error) {
	if ingestBaseURL == "" {
		return "", nil
	}
	key, err := s.EnsureSentryKey(project.Name, func() (string, error) { return RandomHex(16) })
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%s@%s/%d", key, ingestBaseURL, project.ID), nil
}

// sharedEnv builds the POSTGRES_* env vars for project from its linked
// database (if any), followed by the project's "Shared variables" — the
// part every service in the project gets (the app itself, plus any
// worker), as opposed to a service's own variables on top of it.
func sharedEnv(s *store.Store, project store.Project) ([]string, error) {
	var env []string
	if project.LinkedDB != "" {
		dbInfo, err := s.GetDatabase(project.LinkedDB)
		if err != nil {
			return nil, fmt.Errorf("linked database %q not found: %w", project.LinkedDB, err)
		}
		env = append(env,
			"POSTGRES_USER="+dbInfo.DBUser,
			"POSTGRES_PASSWORD="+dbInfo.DBPassword,
			"POSTGRES_DB="+dbInfo.DBName,
			"POSTGRES_HOST="+dbInfo.ContainerName,
			fmt.Sprintf("POSTGRES_PORT=%d", dbInfo.Port),
		)
	}
	storageVars, err := storageEnv(s, project)
	if err != nil {
		return nil, err
	}
	env = append(env, storageVars...)

	env = append(env, parseCustomEnv(project.CustomEnv)...)

	// Auto-inject a working SENTRY_DSN so any Sentry SDK the app already
	// uses reports straight to this hako instance with zero setup —
	// only possible once a public base URL is configured (Settings → Access),
	// since that's the one address a deployed container can reach the agent at.
	if base := config.PublicBaseURL(); base != "" {
		if dsn, err := ProjectSentryDSN(s, project, base); err == nil && dsn != "" {
			env = append(env, "SENTRY_DSN="+dsn)
		}
	}

	return env, nil
}

// AppEnv is what the app container actually runs with: the shared env
// (database + project-level shared variables) plus the app's own
// service-only variables on top, so an app-only var can override a shared
// one if the names collide. Exported so the GitHub push webhook handler
// (cmd/agent.go) uses the exact same env a manual redeploy would.
func AppEnv(s *store.Store, project store.Project) ([]string, error) {
	env, err := sharedEnv(s, project)
	if err != nil {
		return nil, err
	}
	env = append(env, parseCustomEnv(project.AppEnv)...)
	return env, nil
}

func SetProjectAppEnv(s *store.Store, projectName, env string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	return s.SetProjectAppEnv(projectName, env)
}

// parseCustomEnv turns the newline-delimited KEY=VALUE text a user types
// into the "Custom env" textarea into a docker env slice, skipping blank
// lines and comments.
func parseCustomEnv(text string) []string {
	var env []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		env = append(env, line)
	}
	return env
}

// DeployImage runs a true zero-downtime blue/green deploy: it starts
// imageTag as the inactive slot with no public port of its own, health-
// checks it directly on the docker network, and only if that passes does
// it atomically point the project's proxy at the new container — existing
// in-flight requests keep talking to the old one, only new requests see
// the swap. The previous slot is removed only after the swap succeeds. If
// the candidate never turns healthy, the previous slot keeps serving
// untouched and the deploy is reported as failed.
func DeployImage(s *store.Store, project store.Project, imageTag string, env []string, out io.Writer) (string, error) {
	ctx := context.Background()

	oldSlot := project.ActiveSlot
	if oldSlot == "" {
		oldSlot = "blue"
	}
	newSlot := "green"
	if oldSlot == "green" {
		newSlot = "blue"
	}

	candidateName := project.Name + "-" + newSlot

	fmt.Fprintf(out, "starting candidate %s\n", candidateName)
	if _, err := deploy.RunContainerInternal(ctx, imageTag, candidateName, project.ContainerPort, env); err != nil {
		fmt.Fprintln(out, "candidate container failed to start:", err)
		return "", err
	}

	candidateIP, err := deploy.ContainerIP(ctx, candidateName)
	if err != nil {
		fmt.Fprintln(out, "candidate started but has no network address:", err)
		return "", err
	}

	candidateURL := fmt.Sprintf("http://%s:%d%s", candidateIP, project.ContainerPort, healthCheckPath(project))
	fmt.Fprintln(out, "waiting for candidate to become healthy...")
	if !deploy.WaitHealthy(candidateURL, 30, time.Second, healthCheckRequiresOK(project)) {
		deploy.RemoveContainer(ctx, candidateName)
		fmt.Fprintf(out, "candidate failed its health check within 30s — previous version (%s) is still running, untouched\n", oldSlot)
		return "", fmt.Errorf("new deployment failed health check; previous version is still running")
	}
	fmt.Fprintln(out, "candidate is healthy at", candidateURL)

	if _, err := proxy.Ensure(project.Name, project.Port); err != nil {
		fmt.Fprintln(out, "candidate is healthy but the project proxy failed to start:", err)
		return "", err
	}

	target, err := url.Parse(fmt.Sprintf("http://%s:%d", candidateIP, project.ContainerPort))
	if err != nil {
		return "", err
	}
	if err := proxy.SetTarget(project.Name, target); err != nil {
		fmt.Fprintln(out, "candidate is healthy but the traffic switch failed:", err)
		return "", err
	}
	fmt.Fprintln(out, "traffic switched to candidate — zero dropped requests")

	// Only now, with traffic already on the new container, is it safe to
	// retire the old one.
	if err := deploy.RemoveContainer(ctx, project.Name+"-"+oldSlot); err != nil {
		fmt.Fprintln(out, "traffic switched, but failed to remove previous slot:", err)
	}

	if err := s.SetProjectActiveSlot(project.Name, newSlot); err != nil {
		return "", err
	}

	fmt.Fprintf(out, "blue/green deploy: %s -> %s, image %s\n", oldSlot, newSlot, imageTag)
	return candidateName, nil
}

func logDeploy(s *store.Store, project, trigger, status, output string) {
	s.CreateDeployLog(store.DeployLog{Project: project, Trigger: trigger, Status: status, Output: output})
}

func SetProjectDomain(s *store.Store, projectName, domain string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if err := s.SetProjectDomain(projectName, domain); err != nil {
		return err
	}
	return SyncDomainRouting(s)
}

// SetProjectHealthCheckPath changes the path CheckProjectHealth and the
// pre-swap deploy gate hit — takes effect immediately for the uptime
// poller, and on the current running container right away too (it's not
// baked into the container like env vars are), no redeploy needed.
func SetProjectHealthCheckPath(s *store.Store, projectName, path string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	return s.SetProjectHealthCheckPath(projectName, path)
}

// SetProjectCustomEnv saves the project's own env vars. It only takes
// effect on the next deploy — env is baked into a container at start time,
// so an already-running container keeps its old values until redeployed.
func SetProjectCustomEnv(s *store.Store, projectName, env string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	return s.SetProjectCustomEnv(projectName, env)
}

// SyncDomainRouting is a no-op kept for call-site compatibility: domains are
// now just stored on the project, and exposure is bring-your-own — point
// Cloudflare or Tailscale at 127.0.0.1:<project port> (see Settings →
// Access). The port serves our in-process proxy, which is what makes
// blue/green swaps zero-downtime.
func SyncDomainRouting(s *store.Store) error {
	return nil
}

// FindGitHubInstallation resolves which installation of the hako
// GitHub App can access repo, and its clone URL.
func FindGitHubInstallation(appID int64, privateKeyPEM, repo string) (installationID int64, cloneURL string, err error) {
	appJWT, err := github.GenerateAppJWT(appID, privateKeyPEM)
	if err != nil {
		return 0, "", err
	}

	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/installation", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("github api returned status %d", resp.StatusCode)
	}

	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, "", err
	}

	return res.ID, "https://github.com/" + repo + ".git", nil
}

// ScanRepoPresets clones repo to a scratch dir just long enough to detect
// its build presets (railpack vs. Dockerfile, and the app's subdirectory),
// then removes the clone — the real build happens later, from a fresh
// clone, when the project is actually deployed.
func ScanRepoPresets(s *store.Store, repo string) ([]detect.Preset, error) {
	app, err := s.GetGitHubApp()
	if err != nil {
		return nil, fmt.Errorf("run 'hako github connect' first: %w", err)
	}

	installationID, cloneURL, err := FindGitHubInstallation(app.AppID, app.PrivateKey, repo)
	if err != nil {
		return nil, fmt.Errorf("repo not accessible, make sure GitHub App is installed on it: %w", err)
	}

	token, err := github.GetInstallationToken(app.AppID, app.PrivateKey, installationID)
	if err != nil {
		return nil, err
	}

	tmpDir := "data/scan-" + strings.ReplaceAll(repo, "/", "-")
	if err := build.CloneRepo(cloneURL, token, tmpDir, os.Stdout); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	presets, err := detect.Scan(tmpDir)
	if err != nil {
		return nil, err
	}
	if len(presets) == 0 {
		return nil, fmt.Errorf("no supported build presets found in repo")
	}
	return presets, nil
}

// envExampleFilenames are checked in order, in the app's build subdirectory
// first (if any) and then the repo root — the first one found wins.
var envExampleFilenames = []string{".env.example", ".env.sample", ".env.dist", ".env.template", "env.example"}

// FindEnvExampleKeys looks for a committed .env.example-style file in repo
// (via the GitHub Contents API, so it needs no local clone) and returns the
// variable names it declares, so the dashboard can offer them as one-click
// additions instead of the user retyping keys they already wrote once.
// A repo with no example file, or no GitHub App connected yet, is not an
// error — it just means there's nothing to suggest.
func FindEnvExampleKeys(s *store.Store, repo, buildPath string) (file string, keys []string, err error) {
	app, err := s.GetGitHubApp()
	if err != nil {
		return "", nil, nil
	}

	installationID, _, err := FindGitHubInstallation(app.AppID, app.PrivateKey, repo)
	if err != nil {
		return "", nil, nil
	}

	token, err := github.GetInstallationToken(app.AppID, app.PrivateKey, installationID)
	if err != nil {
		return "", nil, nil
	}

	var dirs []string
	if buildPath != "" && buildPath != "." {
		dirs = append(dirs, strings.Trim(buildPath, "/")+"/")
	}
	dirs = append(dirs, "")

	for _, dir := range dirs {
		for _, name := range envExampleFilenames {
			path := dir + name
			content, ok := fetchRepoFile(repo, path, token)
			if !ok {
				continue
			}
			return path, parseEnvKeys(content), nil
		}
	}
	return "", nil, nil
}

// fetchRepoFile reads a single file's content from the GitHub Contents API
// at the repo's default branch.
func fetchRepoFile(repo, path, token string) (content string, ok bool) {
	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/contents/"+path, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var res struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || res.Encoding != "base64" {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(res.Content, "\n", ""))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// parseEnvKeys pulls variable names out of .env-style text: one KEY=value
// per line, blank lines and #-comments ignored, an optional leading
// "export " stripped so shell-style example files work too.
func parseEnvKeys(content string) []string {
	var keys []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.Index(line, "=")
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// CreateProject stores the project. Exposure is bring-your-own: point your
// edge (Cloudflare or Tailscale) at 127.0.0.1:<port>.
func CreateProject(s *store.Store, name, repo, domain string, containerPort int, buildPath, buildStrategy string) error {
	if err := s.CreateProject(name, repo, domain, containerPort, buildPath, buildStrategy); err != nil {
		return err
	}
	if domain != "" {
		return SyncDomainRouting(s)
	}
	return nil
}

// LinkDatabase records which database a project uses. No container/network
// plumbing needed here anymore: every database lives in the one shared
// Postgres service (see EnsurePostgresService), which every project's app
// container already reaches over hako's default network — there's
// nothing per-pair left to wire up. The env vars that make the app
// actually use the database only get injected on the next deploy, same as
// SetProjectCustomEnv.
func LinkDatabase(s *store.Store, projectName, dbName string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	if _, err := s.GetDatabase(dbName); err != nil {
		return fmt.Errorf("database %q not found: %w", dbName, err)
	}
	return s.LinkDatabase(projectName, dbName)
}

// UnlinkDatabase forgets the project/database association — the database
// itself keeps running in the shared service untouched.
func UnlinkDatabase(s *store.Store, projectName string) error {
	if _, err := s.GetProjectByName(projectName); err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	return s.LinkDatabase(projectName, "")
}

// postgresServiceContainer is hako's one shared Postgres container —
// fixed, not per-project, since every project's database lives inside it
// (see PostgresService in internal/store).
const postgresServiceContainer = "hako-postgres"

// EnsurePostgresService starts hako's one shared Postgres container
// the first time any database is needed, generating (and then forgetting
// about) a superuser password the official postgres image requires at
// startup — every later admin operation runs via `docker exec`'s local
// trust auth instead, so the password is never needed again. A second call
// after the service already exists is a no-op; version only matters on
// that first call, since there's only one shared version for every
// project's database from then on.
func EnsurePostgresService(s *store.Store, version string) error {
	if _, err := s.GetPostgresService(); err == nil {
		return nil
	}

	password, err := RandomHex(16)
	if err != nil {
		return err
	}

	ctx := context.Background()
	env := []string{"POSTGRES_PASSWORD=" + password}
	// Mount at /var/lib/postgresql (the parent), not .../data — the
	// official postgres image expects this from 18 onward (which stores
	// data in a version-specific subdirectory for pg_ctlcluster
	// compatibility) and it works the same way on 14-17 too, since either
	// version just creates its own data/ subdirectory inside whatever's
	// mounted there. See https://github.com/docker-library/postgres/issues/37.
	if _, err := deploy.RunServiceContainer(ctx, postgresServiceContainer, "postgres:"+version, env, postgresServiceContainer, "/var/lib/postgresql"); err != nil {
		return err
	}

	if !deploy.WaitHealthyFunc(30, time.Second, func() bool {
		up, _ := deploy.PostgresReady(ctx, postgresServiceContainer)
		return up
	}) {
		return fmt.Errorf("postgres service failed to become ready")
	}

	return s.SavePostgresService(store.PostgresService{Version: version, SuperuserPassword: password})
}

// validPGIdent matches names safe to interpolate as quoted Postgres
// identifiers (CREATE/DROP DATABASE/USER don't accept $placeholders).
var validPGIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// quotePGIdent wraps a validated identifier in double quotes, doubling any
// embedded quote as defense-in-depth (validation above already rejects them).
func quotePGIdent(s string) (string, error) {
	if !validPGIdent.MatchString(s) {
		return "", fmt.Errorf("invalid postgres identifier %q: must match %s", s, validPGIdent.String())
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, nil
}

// quotePGLiteral renders a string literal with single quotes escaped.
func quotePGLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// SafeBuildDir joins workDir with a user-supplied build path, rejecting
// absolute paths and ".." escapes so builds can't leave data/work.
func SafeBuildDir(workDir, buildPath string) (string, error) {
	if buildPath == "" {
		return workDir, nil
	}
	if filepath.IsAbs(buildPath) {
		return "", fmt.Errorf("build path must be relative, got %q", buildPath)
	}
	joined := filepath.Join(workDir, filepath.Clean(buildPath))
	rel, err := filepath.Rel(workDir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build path %q escapes work dir", buildPath)
	}
	return joined, nil
}

// CreateDatabase provisions name as its own logical database and
// non-superuser role inside the shared Postgres service (Temps' "one
// service hosts databases for all your projects" model) — starting that
// service first if this is the very first database ever created.
func CreateDatabase(s *store.Store, name, kind, version string) (string, error) {
	if kind != "postgres" {
		return "", fmt.Errorf("unsupported database kind: %s (only 'postgres' for now)", kind)
	}

	if err := EnsurePostgresService(s, version); err != nil {
		return "", fmt.Errorf("failed to start postgres service: %w", err)
	}

	password, err := RandomHex(16)
	if err != nil {
		return "", err
	}

	ctx := context.Background()
	// A role name has to be a valid SQL identifier and can't collide with
	// another project's — the database name itself already has to be
	// unique (store.CreateDatabase's UNIQUE constraint), so reusing it
	// (with a suffix) keeps this simple instead of inventing a second
	// naming scheme.
	dbUser := name + "_user"
	qUser, err := quotePGIdent(dbUser)
	if err != nil {
		return "", err
	}
	qDB, err := quotePGIdent(name)
	if err != nil {
		return "", err
	}
	if err := deploy.PostgresExec(ctx, postgresServiceContainer, fmt.Sprintf(`CREATE USER %s WITH PASSWORD %s`, qUser, quotePGLiteral(password))); err != nil {
		return "", fmt.Errorf("failed to create database role: %w", err)
	}
	if err := deploy.PostgresExec(ctx, postgresServiceContainer, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, qDB, qUser)); err != nil {
		return "", fmt.Errorf("failed to create database: %w", err)
	}
	// Postgres grants CONNECT on every new database to PUBLIC by default —
	// in a shared service hosting every project's database, that would let
	// any project's role at least connect to (and enumerate) every other
	// project's database. Revoking it means only this database's own owner
	// (and the hako-internal superuser) can connect at all.
	if err := deploy.PostgresExec(ctx, postgresServiceContainer, fmt.Sprintf(`REVOKE CONNECT ON DATABASE %s FROM PUBLIC`, qDB)); err != nil {
		return "", fmt.Errorf("failed to isolate database: %w", err)
	}

	if err := s.CreateDatabase(store.Database{
		Name:          name,
		Kind:          kind,
		ContainerName: postgresServiceContainer,
		DBUser:        dbUser,
		DBPassword:    password,
		DBName:        name,
		Port:          5432,
	}); err != nil {
		return "", err
	}

	return postgresServiceContainer, nil
}

// DeleteProject stops and removes the project's containers and forgets it.
func DeleteProject(s *store.Store, name string) error {
	project, err := s.GetProjectByName(name)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", name, err)
	}

	// Remove both blue/green slots — a failed candidate from a bad deploy
	// may be lingering under the inactive slot's name too.
	if err := deploy.RemoveContainer(context.Background(), project.Name+"-blue"); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}
	if err := deploy.RemoveContainer(context.Background(), project.Name+"-green"); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	proxy.Remove(project.Name)

	if worker, err := s.GetWorker(name); err == nil {
		deploy.RemoveContainer(context.Background(), worker.ContainerName())
		s.DeleteWorker(name)
	}

	if err := s.DeleteProject(name); err != nil {
		return err
	}

	return nil
}

// DeleteDatabase drops name's database and role from the shared Postgres
// service — the service container itself keeps running for every other
// project. Refuses if any project still has it linked, since that would
// silently strand the project with a dangling POSTGRES_HOST.
func DeleteDatabase(s *store.Store, name string) error {
	db, err := s.GetDatabase(name)
	if err != nil {
		return fmt.Errorf("database %q not found: %w", name, err)
	}

	linked, err := s.ProjectsLinkedTo(name)
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return fmt.Errorf("database %q is still linked to project(s) %s — unlink first", name, strings.Join(linked, ", "))
	}

	ctx := context.Background()
	qDropDB, err := quotePGIdent(db.DBName)
	if err != nil {
		return fmt.Errorf("refusing to drop database with unsafe name %q: %w", db.DBName, err)
	}
	qDropUser, err := quotePGIdent(db.DBUser)
	if err != nil {
		return fmt.Errorf("refusing to drop role with unsafe name %q: %w", db.DBUser, err)
	}
	if err := deploy.PostgresExec(ctx, db.ContainerName, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, qDropDB)); err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}
	if err := deploy.PostgresExec(ctx, db.ContainerName, fmt.Sprintf(`DROP USER IF EXISTS %s`, qDropUser)); err != nil {
		return fmt.Errorf("failed to drop database role: %w", err)
	}

	return s.DeleteDatabase(name)
}

// ImageTag is the docker image tag a project's app (and its worker, if
// any) run from — one tag per repo, rebuilt in place on every push or
// manual redeploy, so a worker redeploy can reuse whatever the app last
// built without rebuilding it again.
func ImageTag(project store.Project) string {
	return "hako/" + strings.ReplaceAll(project.Repo, "/", "-") + ":latest"
}

// PreviousImageTag is where SnapshotPreviousImage stashes a project's
// image right before a fresh build overwrites :latest — the one and only
// generation RollbackProject can redeploy.
func PreviousImageTag(project store.Project) string {
	return strings.TrimSuffix(ImageTag(project), ":latest") + ":previous"
}

// SnapshotPreviousImage tags a project's current :latest image as
// :previous, right before a build is about to overwrite :latest — a no-op,
// not an error, the first time a project ever builds (there's nothing yet
// to snapshot).
func SnapshotPreviousImage(ctx context.Context, project store.Project) error {
	tag := ImageTag(project)
	exists, err := deploy.ImageExists(ctx, tag)
	if err != nil || !exists {
		return err
	}
	return deploy.TagImage(ctx, tag, PreviousImageTag(project))
}

// RollbackProject redeploys the image that was running before the most
// recent build, through the same blue/green swap and health-check gate as
// a normal deploy — a candidate that fails health checks doesn't take
// over traffic here either. Only one generation back is ever kept (see
// SnapshotPreviousImage), so this can't step further back than the last
// build.
func RollbackProject(s *store.Store, name string) (string, error) {
	project, err := s.GetProjectByName(name)
	if err != nil {
		return "", fmt.Errorf("project %q not found: %w", name, err)
	}

	previousTag := PreviousImageTag(*project)
	ctx := context.Background()
	exists, err := deploy.ImageExists(ctx, previousTag)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("no previous deploy to roll back to")
	}

	env, err := AppEnv(s, *project)
	if err != nil {
		logDeploy(s, name, "rollback", "failed", err.Error())
		return "", err
	}

	var out strings.Builder
	containerID, err := DeployImage(s, *project, previousTag, env, &out)
	if err != nil {
		logDeploy(s, name, "rollback", "failed", out.String())
		return "", err
	}

	// Keep :latest in sync with what's now actually running, so the next
	// build's SnapshotPreviousImage doesn't overwrite :previous with the
	// broken image this rollback just moved away from.
	if err := deploy.TagImage(ctx, previousTag, ImageTag(*project)); err != nil {
		fmt.Fprintln(&out, "rollback succeeded but failed to resync the :latest tag:", err)
	}

	logDeploy(s, name, "rollback", "success", out.String())
	return containerID, nil
}

// workerEnv gives the worker the same shared env the app gets (database +
// project-level shared variables — a task-queue worker like procrastinate
// needs the same DB the web process does) plus the worker's own
// service-only variables on top, so a worker-specific override wins on
// collision.
func workerEnv(s *store.Store, project store.Project, worker store.Worker) ([]string, error) {
	env, err := sharedEnv(s, project)
	if err != nil {
		return nil, err
	}
	env = append(env, parseCustomEnv(worker.CustomEnv)...)
	return env, nil
}

// DeployWorker (re)starts the project's worker container from imageTag —
// a plain restart, not a blue/green swap, since a background worker isn't
// serving traffic.
func DeployWorker(s *store.Store, project store.Project, worker store.Worker, imageTag string, out io.Writer) (string, error) {
	ctx := context.Background()

	env, err := workerEnv(s, project, worker)
	if err != nil {
		return "", err
	}

	containerName := worker.ContainerName()
	fmt.Fprintf(out, "starting worker %s\n", containerName)
	containerID, err := deploy.RunWorkerContainer(ctx, imageTag, containerName, worker.Command, env)
	if err != nil {
		fmt.Fprintln(out, "worker container failed to start:", err)
		return "", err
	}

	fmt.Fprintf(out, "worker running: %s\n", imageTag)
	return containerID, nil
}

// CreateWorker stores the worker and deploys it immediately from whatever
// image the project's app last built.
func CreateWorker(s *store.Store, projectName, name, command, customEnv string) error {
	project, err := s.GetProjectByName(projectName)
	if err != nil {
		return fmt.Errorf("project %q not found: %w", projectName, err)
	}
	worker := store.Worker{ProjectName: projectName, Name: name, Command: command, CustomEnv: customEnv}
	if err := s.CreateWorker(worker); err != nil {
		return err
	}

	var out strings.Builder
	if _, err := DeployWorker(s, *project, worker, ImageTag(*project), &out); err != nil {
		logDeploy(s, projectName, "manual", "failed", "worker: "+out.String())
		return err
	}
	logDeploy(s, projectName, "manual", "success", "worker: "+out.String())
	return nil
}

// RedeployWorker restarts the worker from the project's current image —
// used both for a manual "Redeploy" and after every app build, so the
// worker's code stays in sync with the app it shares a repo with.
func RedeployWorker(s *store.Store, projectName string) (string, error) {
	project, err := s.GetProjectByName(projectName)
	if err != nil {
		return "", fmt.Errorf("project %q not found: %w", projectName, err)
	}
	worker, err := s.GetWorker(projectName)
	if err != nil {
		return "", fmt.Errorf("project %q has no worker: %w", projectName, err)
	}

	var out strings.Builder
	containerID, err := DeployWorker(s, *project, *worker, ImageTag(*project), &out)
	if err != nil {
		logDeploy(s, projectName, "manual", "failed", "worker: "+out.String())
		return "", err
	}
	logDeploy(s, projectName, "manual", "success", "worker: "+out.String())
	return containerID, nil
}

func SetWorkerCommand(s *store.Store, projectName, command string) error {
	if _, err := s.GetWorker(projectName); err != nil {
		return fmt.Errorf("project %q has no worker: %w", projectName, err)
	}
	return s.SetWorkerCommand(projectName, command)
}

func SetWorkerCustomEnv(s *store.Store, projectName, env string) error {
	if _, err := s.GetWorker(projectName); err != nil {
		return fmt.Errorf("project %q has no worker: %w", projectName, err)
	}
	return s.SetWorkerCustomEnv(projectName, env)
}

// DeleteWorker stops and removes the worker's container and forgets it —
// independent of the database's delete rules, since a worker is never
// shared across projects, so there's nothing to check before removing it.
func DeleteWorker(s *store.Store, projectName string) error {
	worker, err := s.GetWorker(projectName)
	if err != nil {
		return fmt.Errorf("project %q has no worker: %w", projectName, err)
	}
	if err := deploy.RemoveContainer(context.Background(), worker.ContainerName()); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}
	return s.DeleteWorker(projectName)
}

// CheckWorkerHealth records whether the worker's container is currently
// running — there's no HTTP endpoint to probe like a project or database
// has, so "up" just means the container is alive and not stuck
// restart-looping.
func CheckWorkerHealth(s *store.Store, projectName string) (up bool) {
	worker, err := s.GetWorker(projectName)
	if err != nil {
		return false
	}
	status, _, err := deploy.ContainerStatus(context.Background(), worker.ContainerName())
	up = err == nil && status == "running"
	s.RecordHealthCheck("worker:"+projectName, up, 0)
	return up
}
