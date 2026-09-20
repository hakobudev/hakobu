package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Project struct {
	ID            int64
	Name          string
	Repo          string
	Domain        string
	Port          int
	ContainerPort int
	BuildPath     string
	BuildStrategy string
	LinkedDB      string
	ActiveSlot    string
	// CustomEnv is project-level — "Shared variables" in the UI, passed to
	// the app and to its worker (if any). AppEnv is the app's own,
	// service-only on top of that, symmetric to Worker.CustomEnv.
	CustomEnv       string
	AppEnv          string
	SentryKey       string
	HealthCheckPath string
	LinkedStorage   string
}

// ContainerName is the actual docker container currently serving traffic
// for this project — projects deploy blue/green, so this is never just
// the project name.
func (p Project) ContainerName() string {
	slot := p.ActiveSlot
	if slot == "" {
		slot = "blue"
	}
	return p.Name + "-" + slot
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// WAL lets readers and writers run concurrently instead of blocking each
	// other; busy_timeout makes a writer that still collides wait and retry
	// instead of failing immediately with SQLITE_BUSY. Without these, the
	// health poller's periodic writes race with the dashboard's own reads
	// (worse the more the dashboard polls — e.g. the live traffic views).
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		repo TEXT,
		domain TEXT,
		port INTEGER,
		build_path TEXT DEFAULT '',
		build_strategy TEXT DEFAULT 'railpack',
		linked_db TEXT DEFAULT ''
	);`

	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN linked_db TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN container_port INTEGER DEFAULT 8080`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN active_slot TEXT DEFAULT 'blue'`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN custom_env TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN app_env TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	// sentry_key is the public key half of the project's auto-issued DSN
	// (see EnsureSentryKey) — the projects.id column itself doubles as the
	// DSN's project_id segment, so no separate id is needed.
	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN sentry_key TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	// health_check_path is the path used both for the pre-swap deploy gate
	// (DeployImage) and the background uptime poller (CheckProjectHealth) —
	// defaults to "/" so existing projects keep behaving exactly as before.
	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN health_check_path TEXT DEFAULT '/'`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	// linked_storage names a row in the storages table (see CreateStorage) —
	// same shape as linked_db, but object storage credentials need no
	// docker-network wiring, just env injection, so there's no equivalent
	// of LinkDatabase's network-attach step.
	if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN linked_storage TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return nil, err
	}

	return &Store{db: db}, nil
}

const basePort = 8081

func (s *Store) CreateProject(name, repo, domain string, containerPort int, buildPath, buildStrategy string) error {
	var maxPort sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(port) FROM projects`).Scan(&maxPort); err != nil {
		return err
	}
	port := basePort
	if maxPort.Valid && int(maxPort.Int64) >= basePort {
		port = int(maxPort.Int64) + 1
	}

	_, err := s.db.Exec(
		`INSERT INTO projects (name, repo, domain, port, container_port, build_path, build_strategy) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, repo, domain, port, containerPort, buildPath, buildStrategy,
	)
	return err
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, repo, domain, port, container_port, linked_db, active_slot, health_check_path, linked_storage FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Domain, &p.Port, &p.ContainerPort, &p.LinkedDB, &p.ActiveSlot, &p.HealthCheckPath, &p.LinkedStorage); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, nil
}

func (s *Store) SaveGitHubApp(appID int64, slug, pem, webhookSecret, clientID, clientSecret string) error {
	schema := `
	CREATE TABLE IF NOT EXISTS github_app (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		app_id INTEGER NOT NULL,
		slug TEXT NOT NULL,
		private_key TEXT NOT NULL,
		webhook_secret TEXT NOT NULL,
		client_id TEXT NOT NULL,
		client_secret TEXT NOT NULL
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO github_app (id, app_id, slug, private_key, webhook_secret, client_id, client_secret)
		 VALUES (1, ?, ?, ?, ?, ?, ?)`,
		appID, slug, pem, webhookSecret, clientID, clientSecret,
	)
	return err
}

type GitHubApp struct {
	AppID         int64
	Slug          string
	PrivateKey    string
	WebhookSecret string
	ClientID      string
	ClientSecret  string
}

func (s *Store) GetGitHubApp() (*GitHubApp, error) {
	var a GitHubApp
	err := s.db.QueryRow(
		`SELECT app_id, slug, private_key, webhook_secret, client_id, client_secret FROM github_app WHERE id = 1`,
	).Scan(&a.AppID, &a.Slug, &a.PrivateKey, &a.WebhookSecret, &a.ClientID, &a.ClientSecret)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Storage is one named S3-compatible bucket credential set -- a list-based
// resource like Database: create as many as you actually have buckets for
// (often just one), then point database backups and/or a project's own
// storage link at whichever one makes sense, the same way a project picks
// one database to link from the list of databases. Provider "r2" derives
// its endpoint from AccountID (matching Cloudflare's own
// <account_id>.r2.cloudflarestorage.com convention, and the
// STORAGE_PROVIDER / R2_ACCOUNT_ID env vars an app like kasl-crm already
// expects); provider "s3" uses Endpoint as given (AWS itself, MinIO, or
// anything else S3-compatible).
type Storage struct {
	Name            string
	Provider        string // "r2", "s3", or "rustfs"
	AccountID       string // r2 only
	Endpoint        string // s3/rustfs
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Region          string
}

func (s *Store) initStorageTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS storages (
		name TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		account_id TEXT NOT NULL DEFAULT '',
		endpoint TEXT NOT NULL DEFAULT '',
		access_key_id TEXT NOT NULL,
		secret_access_key TEXT NOT NULL,
		bucket TEXT NOT NULL,
		region TEXT NOT NULL DEFAULT 'auto'
	);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) CreateStorage(st Storage) error {
	if err := s.initStorageTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO storages (name, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		st.Name, st.Provider, st.AccountID, st.Endpoint, st.AccessKeyID, st.SecretAccessKey, st.Bucket, st.Region,
	)
	return err
}

// SaveStorage is CreateStorage's upsert-by-name counterpart, used to sync
// a storage from environment variables on every agent startup without
// erroring on the second run.
func (s *Store) SaveStorage(st Storage) error {
	if err := s.initStorageTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO storages (name, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		st.Name, st.Provider, st.AccountID, st.Endpoint, st.AccessKeyID, st.SecretAccessKey, st.Bucket, st.Region,
	)
	return err
}

func (s *Store) GetStorage(name string) (*Storage, error) {
	if err := s.initStorageTable(); err != nil {
		return nil, err
	}
	var st Storage
	err := s.db.QueryRow(
		`SELECT name, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region FROM storages WHERE name = ?`,
		name,
	).Scan(&st.Name, &st.Provider, &st.AccountID, &st.Endpoint, &st.AccessKeyID, &st.SecretAccessKey, &st.Bucket, &st.Region)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) ListStorages() ([]Storage, error) {
	if err := s.initStorageTable(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT name, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region FROM storages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var storages []Storage
	for rows.Next() {
		var st Storage
		if err := rows.Scan(&st.Name, &st.Provider, &st.AccountID, &st.Endpoint, &st.AccessKeyID, &st.SecretAccessKey, &st.Bucket, &st.Region); err != nil {
			return nil, err
		}
		storages = append(storages, st)
	}
	return storages, nil
}

func (s *Store) DeleteStorage(name string) error {
	_, err := s.db.Exec(`DELETE FROM storages WHERE name = ?`, name)
	return err
}

// ProjectsLinkedToStorage lists project names with this storage linked --
// mirrors ProjectsLinkedTo (databases), used the same way to refuse
// deleting a storage still in use.
func (s *Store) ProjectsLinkedToStorage(name string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM projects WHERE linked_storage = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, nil
}

func (s *Store) SetProjectLinkedStorage(projectName, storageName string) error {
	_, err := s.db.Exec(`UPDATE projects SET linked_storage = ? WHERE name = ?`, storageName, projectName)
	return err
}

// Backup records one successful database dump uploaded to R2 — the object
// itself is the source of truth for content, this row is just a fast local
// index so the dashboard can list backups without an R2 API round trip.
type Backup struct {
	ID        int64
	Database  string
	ObjectKey string
	SizeBytes int64
	CreatedAt string
}

func (s *Store) CreateBackup(b Backup) error {
	schema := `
	CREATE TABLE IF NOT EXISTS backups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		database TEXT NOT NULL,
		object_key TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	_, err := s.db.Exec(
		`INSERT INTO backups (database, object_key, size_bytes) VALUES (?, ?, ?)`,
		b.Database, b.ObjectKey, b.SizeBytes,
	)
	return err
}

func (s *Store) ListBackups(database string, limit int) ([]Backup, error) {
	schema := `
	CREATE TABLE IF NOT EXISTS backups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		database TEXT NOT NULL,
		object_key TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, database, object_key, size_bytes, created_at FROM backups WHERE database = ? ORDER BY id DESC LIMIT ?`,
		database, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backups []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.Database, &b.ObjectKey, &b.SizeBytes, &b.CreatedAt); err != nil {
			return nil, err
		}
		backups = append(backups, b)
	}
	return backups, nil
}

func (s *Store) GetProjectByName(name string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, repo, domain, port, container_port, build_path, build_strategy, linked_db, active_slot, custom_env, app_env, sentry_key, health_check_path, linked_storage FROM projects WHERE name = ?`,
		name,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Domain, &p.Port, &p.ContainerPort, &p.BuildPath, &p.BuildStrategy, &p.LinkedDB, &p.ActiveSlot, &p.CustomEnv, &p.AppEnv, &p.SentryKey, &p.HealthCheckPath, &p.LinkedStorage)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) GetProjectByRepo(repo string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, repo, domain, port, container_port, build_path, build_strategy, linked_db, active_slot, custom_env, app_env, sentry_key, health_check_path, linked_storage FROM projects WHERE repo = ?`,
		repo,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Domain, &p.Port, &p.ContainerPort, &p.BuildPath, &p.BuildStrategy, &p.LinkedDB, &p.ActiveSlot, &p.CustomEnv, &p.AppEnv, &p.SentryKey, &p.HealthCheckPath, &p.LinkedStorage)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProjectByDomain looks up the project whose custom domain matches host —
// used by the automatic-HTTPS edge listener to route each incoming TLS
// connection/request by SNI/Host without keeping a separate routing table.
func (s *Store) GetProjectByDomain(domain string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, repo, domain, port, container_port, build_path, build_strategy, linked_db, active_slot, custom_env, app_env, sentry_key, health_check_path, linked_storage FROM projects WHERE domain = ?`,
		domain,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Domain, &p.Port, &p.ContainerPort, &p.BuildPath, &p.BuildStrategy, &p.LinkedDB, &p.ActiveSlot, &p.CustomEnv, &p.AppEnv, &p.SentryKey, &p.HealthCheckPath, &p.LinkedStorage)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProjectBySentryProjectID looks up a project by the numeric id used as
// the "project_id" path segment in its auto-issued Sentry DSN — the
// projects table's own primary key, reused so no separate id needs
// tracking.
func (s *Store) GetProjectBySentryProjectID(id string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, repo, domain, port, container_port, build_path, build_strategy, linked_db, active_slot, custom_env, app_env, sentry_key, health_check_path, linked_storage FROM projects WHERE id = ?`,
		id,
	).Scan(&p.ID, &p.Name, &p.Repo, &p.Domain, &p.Port, &p.ContainerPort, &p.BuildPath, &p.BuildStrategy, &p.LinkedDB, &p.ActiveSlot, &p.CustomEnv, &p.AppEnv, &p.SentryKey, &p.HealthCheckPath, &p.LinkedStorage)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// EnsureSentryKey returns the project's DSN public key, generating and
// persisting one via generate() the first time it's needed — mirrors
// GetOrCreateAPIToken's lazy-generate pattern.
func (s *Store) EnsureSentryKey(projectName string, generate func() (string, error)) (string, error) {
	var key string
	if err := s.db.QueryRow(`SELECT sentry_key FROM projects WHERE name = ?`, projectName).Scan(&key); err != nil {
		return "", err
	}
	if key != "" {
		return key, nil
	}

	key, err := generate()
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`UPDATE projects SET sentry_key = ? WHERE name = ?`, key, projectName); err != nil {
		return "", err
	}
	return key, nil
}

func (s *Store) SetProjectContainerPort(projectName string, port int) error {
	_, err := s.db.Exec(`UPDATE projects SET container_port = ? WHERE name = ?`, port, projectName)
	return err
}

func (s *Store) SetProjectCustomEnv(projectName, env string) error {
	_, err := s.db.Exec(`UPDATE projects SET custom_env = ? WHERE name = ?`, env, projectName)
	return err
}

func (s *Store) SetProjectAppEnv(projectName, env string) error {
	_, err := s.db.Exec(`UPDATE projects SET app_env = ? WHERE name = ?`, env, projectName)
	return err
}

func (s *Store) SetProjectActiveSlot(projectName, slot string) error {
	_, err := s.db.Exec(`UPDATE projects SET active_slot = ? WHERE name = ?`, slot, projectName)
	return err
}

func (s *Store) SetProjectHealthCheckPath(projectName, path string) error {
	_, err := s.db.Exec(`UPDATE projects SET health_check_path = ? WHERE name = ?`, path, projectName)
	return err
}

func (s *Store) initDBTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS databases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		kind TEXT NOT NULL,
		container_name TEXT NOT NULL,
		db_user TEXT NOT NULL,
		db_password TEXT NOT NULL,
		db_name TEXT NOT NULL,
		port INTEGER NOT NULL
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// backup_storage names a row in the storages table (see CreateStorage) —
	// picked per database, right where its backups live, rather than one
	// instance-wide pointer: different databases often want different
	// buckets.
	if _, err := s.db.Exec(`ALTER TABLE databases ADD COLUMN backup_storage TEXT DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

type Database struct {
	ID            int64
	Name          string
	Kind          string
	ContainerName string
	DBUser        string
	DBPassword    string
	DBName        string
	Port          int
	BackupStorage string
}

// PostgresService is hako's one shared Postgres container — every
// Database row's ContainerName points at it (see ops.EnsurePostgresService
// and ops.CreateDatabase), each with its own logical database and
// non-superuser role inside it, instead of a dedicated container per
// project (Temps' "one service hosts databases for all your projects"
// model). SuperuserPassword is only ever used to satisfy the official
// postgres image's startup requirement — hako always administers the
// service via `docker exec` from inside the container, which uses local
// trust auth, so the password itself never needs to be typed anywhere
// again after container creation.
type PostgresService struct {
	Version           string
	SuperuserPassword string
}

func (s *Store) initPostgresServiceTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS postgres_service (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		version TEXT NOT NULL,
		superuser_password TEXT NOT NULL
	);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) SavePostgresService(ps PostgresService) error {
	if err := s.initPostgresServiceTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO postgres_service (id, version, superuser_password) VALUES (1, ?, ?)`,
		ps.Version, ps.SuperuserPassword,
	)
	return err
}

func (s *Store) GetPostgresService() (*PostgresService, error) {
	if err := s.initPostgresServiceTable(); err != nil {
		return nil, err
	}
	var ps PostgresService
	err := s.db.QueryRow(`SELECT version, superuser_password FROM postgres_service WHERE id = 1`).
		Scan(&ps.Version, &ps.SuperuserPassword)
	if err != nil {
		return nil, err
	}
	return &ps, nil
}

func (s *Store) CreateDatabase(d Database) error {
	if err := s.initDBTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO databases (name, kind, container_name, db_user, db_password, db_name, port) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.Name, d.Kind, d.ContainerName, d.DBUser, d.DBPassword, d.DBName, d.Port,
	)
	return err
}

func (s *Store) ListDatabases() ([]Database, error) {
	if err := s.initDBTable(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, name, kind, container_name, db_user, db_password, db_name, port, backup_storage FROM databases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []Database
	for rows.Next() {
		var d Database
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.ContainerName, &d.DBUser, &d.DBPassword, &d.DBName, &d.Port, &d.BackupStorage); err != nil {
			return nil, err
		}
		databases = append(databases, d)
	}
	return databases, nil
}

func (s *Store) GetDatabase(name string) (*Database, error) {
	var d Database
	err := s.db.QueryRow(
		`SELECT id, name, kind, container_name, db_user, db_password, db_name, port, backup_storage FROM databases WHERE name = ?`,
		name,
	).Scan(&d.ID, &d.Name, &d.Kind, &d.ContainerName, &d.DBUser, &d.DBPassword, &d.DBName, &d.Port, &d.BackupStorage)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) SetDatabaseBackupStorage(dbName, storageName string) error {
	_, err := s.db.Exec(`UPDATE databases SET backup_storage = ? WHERE name = ?`, storageName, dbName)
	return err
}

func (s *Store) initWorkerTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS workers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_name TEXT UNIQUE NOT NULL,
		name TEXT NOT NULL,
		command TEXT NOT NULL,
		custom_env TEXT DEFAULT ''
	);`
	_, err := s.db.Exec(schema)
	return err
}

// Worker is a background process deployed from the same image as its
// project's app — same repo/build, no exposed port, its own start command
// (e.g. a task-queue worker) instead of the web server's. One per project.
type Worker struct {
	ID          int64
	ProjectName string
	Name        string
	Command     string
	CustomEnv   string
}

// ContainerName has no blue/green slot — a worker doesn't serve traffic,
// so a plain restart (not a zero-downtime swap) is an acceptable way to
// roll out a new version.
func (w Worker) ContainerName() string {
	return w.ProjectName + "-worker"
}

func (s *Store) CreateWorker(w Worker) error {
	if err := s.initWorkerTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO workers (project_name, name, command, custom_env) VALUES (?, ?, ?, ?)`,
		w.ProjectName, w.Name, w.Command, w.CustomEnv,
	)
	return err
}

func (s *Store) GetWorker(projectName string) (*Worker, error) {
	if err := s.initWorkerTable(); err != nil {
		return nil, err
	}
	var w Worker
	err := s.db.QueryRow(
		`SELECT id, project_name, name, command, custom_env FROM workers WHERE project_name = ?`,
		projectName,
	).Scan(&w.ID, &w.ProjectName, &w.Name, &w.Command, &w.CustomEnv)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) SetWorkerCommand(projectName, command string) error {
	_, err := s.db.Exec(`UPDATE workers SET command = ? WHERE project_name = ?`, command, projectName)
	return err
}

func (s *Store) SetWorkerCustomEnv(projectName, env string) error {
	_, err := s.db.Exec(`UPDATE workers SET custom_env = ? WHERE project_name = ?`, env, projectName)
	return err
}

func (s *Store) DeleteWorker(projectName string) error {
	if err := s.initWorkerTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM workers WHERE project_name = ?`, projectName)
	return err
}

func (s *Store) GetOrCreateAPIToken(generate func() (string, error)) (string, error) {
	schema := `
	CREATE TABLE IF NOT EXISTS api_token (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		token TEXT NOT NULL
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return "", err
	}

	var token string
	err := s.db.QueryRow(`SELECT token FROM api_token WHERE id = 1`).Scan(&token)
	if err == nil {
		return token, nil
	}

	token, err = generate()
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT INTO api_token (id, token) VALUES (1, ?)`, token); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) SetProjectDomain(projectName, domain string) error {
	_, err := s.db.Exec(`UPDATE projects SET domain = ? WHERE name = ?`, domain, projectName)
	return err
}

type DeployLog struct {
	ID        int64
	Project   string
	Trigger   string
	Status    string
	Output    string
	CreatedAt string
}

func (s *Store) CreateDeployLog(d DeployLog) error {
	schema := `
	CREATE TABLE IF NOT EXISTS deploy_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		trigger_source TEXT NOT NULL,
		status TEXT NOT NULL,
		output TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	_, err := s.db.Exec(
		`INSERT INTO deploy_logs (project, trigger_source, status, output) VALUES (?, ?, ?, ?)`,
		d.Project, d.Trigger, d.Status, d.Output,
	)
	return err
}

// TelemetryEvent is one error or log entry received through a project's
// Sentry-compatible ingestion endpoint (see cmd/ingest.go). Payload keeps
// the item's raw JSON so a later detail view can show stack traces /
// attributes without a schema change every time Sentry's SDKs add a field.
type TelemetryEvent struct {
	ID        int64
	Project   string
	Kind      string // "error" or "log"
	Level     string
	Message   string
	Payload   string
	CreatedAt string
}

func (s *Store) CreateTelemetryEvent(e TelemetryEvent) error {
	schema := `
	CREATE TABLE IF NOT EXISTS telemetry_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		kind TEXT NOT NULL,
		level TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	_, err := s.db.Exec(
		`INSERT INTO telemetry_events (project, kind, level, message, payload) VALUES (?, ?, ?, ?, ?)`,
		e.Project, e.Kind, e.Level, e.Message, e.Payload,
	)
	return err
}

// ListTelemetryEvents returns a project's most recent errors/logs, newest
// first, optionally filtered to a single kind ("error" or "log"); an empty
// kind returns both.
func (s *Store) ListTelemetryEvents(project, kind string, limit int) ([]TelemetryEvent, error) {
	schema := `
	CREATE TABLE IF NOT EXISTS telemetry_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		kind TEXT NOT NULL,
		level TEXT NOT NULL DEFAULT '',
		message TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return nil, err
	}

	query := `SELECT id, project, kind, level, message, payload, created_at FROM telemetry_events WHERE project = ?`
	args := []interface{}{project}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []TelemetryEvent
	for rows.Next() {
		var e TelemetryEvent
		if err := rows.Scan(&e.ID, &e.Project, &e.Kind, &e.Level, &e.Message, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

func (s *Store) ListDeployLogs(project string, limit int) ([]DeployLog, error) {
	schema := `
	CREATE TABLE IF NOT EXISTS deploy_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project TEXT NOT NULL,
		trigger_source TEXT NOT NULL,
		status TEXT NOT NULL,
		output TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);`
	if _, err := s.db.Exec(schema); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, project, trigger_source, status, output, created_at FROM deploy_logs WHERE project = ? ORDER BY id DESC LIMIT ?`,
		project, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []DeployLog
	for rows.Next() {
		var d DeployLog
		if err := rows.Scan(&d.ID, &d.Project, &d.Trigger, &d.Status, &d.Output, &d.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, d)
	}
	return logs, nil
}

type HealthCheck struct {
	ID         int64
	Target     string
	Up         bool
	ResponseMs int
	CheckedAt  time.Time
}

func (s *Store) initHealthChecksTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS health_checks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target TEXT NOT NULL,
		up INTEGER NOT NULL,
		response_ms INTEGER NOT NULL,
		checked_at TEXT NOT NULL
	);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) RecordHealthCheck(target string, up bool, responseMs int) error {
	if err := s.initHealthChecksTable(); err != nil {
		return err
	}
	upInt := 0
	if up {
		upInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO health_checks (target, up, response_ms, checked_at) VALUES (?, ?, ?, ?)`,
		target, upInt, responseMs, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// ListHealthChecks returns up to limit checks for target, oldest first (so
// callers can render them left-to-right like an uptime bar).
func (s *Store) ListHealthChecks(target string, limit int) ([]HealthCheck, error) {
	if err := s.initHealthChecksTable(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT id, target, up, response_ms, checked_at FROM health_checks WHERE target = ? ORDER BY id DESC LIMIT ?`,
		target, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checks []HealthCheck
	for rows.Next() {
		var c HealthCheck
		var upInt int
		var checkedAt string
		if err := rows.Scan(&c.ID, &c.Target, &upInt, &c.ResponseMs, &checkedAt); err != nil {
			return nil, err
		}
		c.Up = upInt == 1
		c.CheckedAt, _ = time.Parse(time.RFC3339, checkedAt)
		checks = append(checks, c)
	}

	for i, j := 0, len(checks)-1; i < j; i, j = i+1, j-1 {
		checks[i], checks[j] = checks[j], checks[i]
	}
	return checks, nil
}

type ResourceSample struct {
	ID         int64
	Target     string
	CPUPercent float64
	MemUsedMB  float64
	SampledAt  time.Time
}

func (s *Store) initResourceSamplesTable() error {
	schema := `
	CREATE TABLE IF NOT EXISTS resource_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target TEXT NOT NULL,
		cpu_percent REAL NOT NULL,
		mem_used_mb REAL NOT NULL,
		sampled_at TEXT NOT NULL
	);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) RecordResourceSample(target string, cpuPercent, memUsedMB float64) error {
	if err := s.initResourceSamplesTable(); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO resource_samples (target, cpu_percent, mem_used_mb, sampled_at) VALUES (?, ?, ?, ?)`,
		target, cpuPercent, memUsedMB, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// ListResourceSamples returns up to limit samples for target, oldest first,
// for sparkline rendering.
func (s *Store) ListResourceSamples(target string, limit int) ([]ResourceSample, error) {
	if err := s.initResourceSamplesTable(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT id, target, cpu_percent, mem_used_mb, sampled_at FROM resource_samples WHERE target = ? ORDER BY id DESC LIMIT ?`,
		target, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []ResourceSample
	for rows.Next() {
		var r ResourceSample
		var sampledAt string
		if err := rows.Scan(&r.ID, &r.Target, &r.CPUPercent, &r.MemUsedMB, &sampledAt); err != nil {
			return nil, err
		}
		r.SampledAt, _ = time.Parse(time.RFC3339, sampledAt)
		samples = append(samples, r)
	}

	for i, j := 0, len(samples)-1; i < j; i, j = i+1, j-1 {
		samples[i], samples[j] = samples[j], samples[i]
	}
	return samples, nil
}

// PruneOldData deletes time-series rows older than retentionDays and
// vacuums to reclaim space. Timestamps are ISO/RFC3339 strings, so a
// lexicographic comparison against strftime(...) works. Tables are created
// lazily, so missing tables are not an error.
func (s *Store) PruneOldData(retentionDays int) error {
	cutoff := fmt.Sprintf("-%d days", retentionDays)
	prunes := []struct{ table, col string }{
		{"telemetry_events", "created_at"},
		{"health_checks", "checked_at"},
		{"resource_samples", "sampled_at"},
		{"deploy_logs", "created_at"},
	}
	for _, p := range prunes {
		_, err := s.db.Exec(fmt.Sprintf(
			`DELETE FROM %s WHERE %s < strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ', 'now', ?)`,
			p.table, p.col), cutoff)
		if err != nil && !strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("prune %s: %w", p.table, err)
		}
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

func (s *Store) LinkDatabase(projectName, dbName string) error {	_, err := s.db.Exec(`UPDATE projects SET linked_db = ? WHERE name = ?`, dbName, projectName)
	return err
}

// DeleteProject removes the project row and any of its recorded
// history (deploy logs, health checks, resource samples).
func (s *Store) DeleteProject(name string) error {
	if _, err := s.db.Exec(`DELETE FROM projects WHERE name = ?`, name); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM deploy_logs WHERE project = ?`, name); err != nil {
		return err
	}
	target := "project:" + name
	if _, err := s.db.Exec(`DELETE FROM health_checks WHERE target = ?`, target); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM resource_samples WHERE target = ?`, target); err != nil {
		return err
	}
	return nil
}

// ProjectsLinkedTo returns the names of projects that have dbName linked,
// so a database can't be deleted out from under a project still using it.
func (s *Store) ProjectsLinkedTo(dbName string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM projects WHERE linked_db = ?`, dbName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, nil
}

// DeleteDatabase removes the database row and any of its recorded history.
func (s *Store) DeleteDatabase(name string) error {
	if _, err := s.db.Exec(`DELETE FROM databases WHERE name = ?`, name); err != nil {
		return err
	}
	target := "database:" + name
	if _, err := s.db.Exec(`DELETE FROM health_checks WHERE target = ?`, target); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM resource_samples WHERE target = ?`, target); err != nil {
		return err
	}
	return nil
}
