CREATE TABLE projects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	shared_env TEXT NOT NULL DEFAULT ''
);

CREATE TABLE apps (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	name TEXT UNIQUE NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	domain TEXT NOT NULL DEFAULT '',
	port INTEGER NOT NULL,
	container_port INTEGER NOT NULL DEFAULT 8080,
	build_path TEXT NOT NULL DEFAULT '',
	build_strategy TEXT NOT NULL DEFAULT 'railpack',
	active_slot TEXT NOT NULL DEFAULT 'blue',
	env TEXT NOT NULL DEFAULT '',
	sentry_key TEXT NOT NULL DEFAULT '',
	health_check_path TEXT NOT NULL DEFAULT '/',
	linked_db TEXT NOT NULL DEFAULT '',
	linked_storage TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_apps_repo ON apps(repo);

CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.port, a.container_port,
	a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key, a.health_check_path,
	a.linked_db, a.linked_storage
FROM apps a JOIN projects p ON p.id = a.project_id;

CREATE TABLE workers (
	app_name TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	command TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT ''
);

CREATE TABLE databases (
	name TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	db_user TEXT NOT NULL,
	db_password TEXT NOT NULL,
	backup_storage TEXT NOT NULL DEFAULT ''
);

CREATE TABLE storages (
	name TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	provider TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	endpoint TEXT NOT NULL DEFAULT '',
	access_key_id TEXT NOT NULL,
	secret_access_key TEXT NOT NULL,
	bucket TEXT NOT NULL,
	region TEXT NOT NULL DEFAULT 'auto'
);

CREATE TABLE backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	database TEXT NOT NULL,
	object_key TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE github_app (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	app_id INTEGER NOT NULL,
	slug TEXT NOT NULL,
	private_key TEXT NOT NULL,
	webhook_secret TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL
);

CREATE TABLE access_control (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	owner_login TEXT NOT NULL DEFAULT '',
	allowed_logins TEXT NOT NULL DEFAULT ''
);

CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	github_login TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE TABLE deploy_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	trigger_source TEXT NOT NULL,
	status TEXT NOT NULL,
	output TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE telemetry_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	kind TEXT NOT NULL,
	level TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
