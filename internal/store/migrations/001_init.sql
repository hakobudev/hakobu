-- Who may sign in, known by GitHub's numeric ID (a login can be renamed
-- and then taken by someone else; it's only shown). The first user is the
-- admin: the panel's own settings (updates, its backups, its Cloudflare,
-- notifications, the master key) and invites are theirs. Anyone else joins
-- with an invite, or signs up where the panel lets them, and sees only what
-- they own. notify_email is where their own alerts go (their apps' failed
-- deploys, crashes, backups): an address confirmed in the panel's
-- Cloudflare account, which sends them; '' for an admin means the panel's
-- address, for anyone else none.
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	github_id INTEGER UNIQUE NOT NULL,
	github_login TEXT NOT NULL,
	github_email TEXT NOT NULL DEFAULT '',
	admin INTEGER NOT NULL DEFAULT 0,
	notify_email TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- An invite is a one-time link; secret_hash is the SHA-256 of its secret.
CREATE TABLE invites (
	secret_hash TEXT PRIMARY KEY,
	created_by INTEGER NOT NULL REFERENCES users(id),
	expires_at TEXT NOT NULL
);

-- id is the SHA-256 of the session cookie; github_id the signed-in account.
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	github_id INTEGER NOT NULL,
	signed_in_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
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

-- GitHub webhook deliveries already handled, so a captured one sent again
-- doesn't deploy again. Kept a few days; older pushes are refused anyway.
CREATE TABLE webhook_deliveries (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- The panel's own Cloudflare account, connected at setup: its tunnel and
-- the panel's DNS record, its email, its watchdog, and the backups of the
-- projects in it.
CREATE TABLE cloudflare (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	api_token TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	tunnel_id TEXT NOT NULL DEFAULT '',
	tunnel_token TEXT NOT NULL DEFAULT '',
	panel_zone_id TEXT NOT NULL DEFAULT '',
	panel_record_id TEXT NOT NULL DEFAULT '',
	backup_bucket TEXT NOT NULL DEFAULT ''
);

-- Users' Cloudflare accounts: a project in one gets its domains, storages
-- and backups there. A user's token connects every account it reaches; the
-- accounts it connected share it under token_group and follow it as its
-- reach changes. name is hakobu's own key for the account (paths,
-- container names), never shown; title is what Cloudflare calls it and
-- domains its domains, kept up to date. tunnel_* is its tunnel on the
-- panel's server, made with its first domain there; backup_bucket is ''
-- until its first backup.
CREATE TABLE cloudflare_accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	user_id INTEGER NOT NULL,
	api_token TEXT NOT NULL,
	token_group TEXT NOT NULL,
	account_id TEXT UNIQUE NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	domains TEXT NOT NULL DEFAULT '',
	tunnel_id TEXT NOT NULL DEFAULT '',
	tunnel_token TEXT NOT NULL DEFAULT '',
	backup_bucket TEXT NOT NULL DEFAULT ''
);

-- Servers other than the panel's that run projects (internal/node,
-- internal/link). A node joins with a one-time token: join_secret_hash is
-- the SHA-256 of its secret until then, '' after; public_key is the
-- node's ed25519 key (hex) from then on.
CREATE TABLE nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	user_id INTEGER NOT NULL DEFAULT 0,
	public_key TEXT NOT NULL DEFAULT '',
	join_secret_hash TEXT NOT NULL DEFAULT '',
	join_expires TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	last_seen TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- The panel's own key on the link, which nodes check it by. It's kept in
-- the panel's database, so a panel brought back on a new server is still
-- the one its nodes joined.
CREATE TABLE link_key (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	private_key TEXT NOT NULL
);

-- Tunnels on servers other than the panel's: one per Cloudflare account
-- with projects there (cloudflare_account_id 0 is the panel's account), as
-- one tunnel can't reach two servers.
CREATE TABLE server_tunnels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id INTEGER NOT NULL REFERENCES nodes(id),
	cloudflare_account_id INTEGER NOT NULL DEFAULT 0,
	tunnel_id TEXT NOT NULL,
	tunnel_token TEXT NOT NULL,
	UNIQUE (node_id, cloudflare_account_id)
);

-- node_id NULL is the panel's own server, cloudflare_account_id NULL the
-- panel's account.
CREATE TABLE projects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	user_id INTEGER NOT NULL DEFAULT 0,
	shared_env TEXT NOT NULL DEFAULT '',
	cloudflare_account_id INTEGER REFERENCES cloudflare_accounts(id),
	node_id INTEGER REFERENCES nodes(id)
);

-- port is the app's proxy on 127.0.0.1; container_port 0 means "detect",
-- and live_port is the port the running container was found listening on.
-- dns_* is the record hakobu owns for domain. memory_mb/cpus 0: no limit.
-- An app with volumes deploys by stopping the old version first, unless
-- share_volumes lets both versions use them during the switch.
-- snapshot_db/snapshot_at describe the dump of the app's database taken
-- before the deploy of its live image, so Rollback can return the data
-- together with the previous image. stack is what its last build was made
-- from, e.g. "Python · FastAPI" ('' if unknown); start_command runs in
-- place of its image's own start ('': the image's).
CREATE TABLE apps (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	name TEXT UNIQUE NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	domain TEXT NOT NULL DEFAULT '',
	dns_zone_id TEXT NOT NULL DEFAULT '',
	dns_record_id TEXT NOT NULL DEFAULT '',
	port INTEGER NOT NULL,
	container_port INTEGER NOT NULL DEFAULT 0,
	live_port INTEGER NOT NULL DEFAULT 0,
	build_path TEXT NOT NULL DEFAULT '',
	build_strategy TEXT NOT NULL DEFAULT 'railpack',
	active_slot TEXT NOT NULL DEFAULT 'blue',
	env TEXT NOT NULL DEFAULT '',
	sentry_key TEXT NOT NULL DEFAULT '',
	health_check_path TEXT NOT NULL DEFAULT '/',
	linked_db TEXT NOT NULL DEFAULT '',
	linked_storage TEXT NOT NULL DEFAULT '',
	share_volumes INTEGER NOT NULL DEFAULT 0,
	memory_mb INTEGER NOT NULL DEFAULT 0,
	cpus REAL NOT NULL DEFAULT 0,
	snapshot_db TEXT NOT NULL DEFAULT '',
	snapshot_at TEXT NOT NULL DEFAULT '',
	stack TEXT NOT NULL DEFAULT '',
	start_command TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_apps_repo ON apps(repo);

CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage, a.share_volumes, a.memory_mb, a.cpus,
	a.snapshot_db, a.snapshot_at, a.stack, a.start_command
FROM apps a JOIN projects p ON p.id = a.project_id;

-- Paths of an app's address served by another app of its project, through
-- the tunnel: shop.example.com/api/... goes to the api app, the rest to
-- the shop. path is a prefix like /api; the target sees the whole path.
CREATE TABLE app_routes (
	app_name TEXT NOT NULL,
	path TEXT NOT NULL,
	target TEXT NOT NULL,
	PRIMARY KEY (app_name, path)
);

-- Apps whose new build went live on a server that dropped off before it
-- became the Latest image there: the panel finishes it (node Promote) as
-- the server connects again, so Latest is the build that serves.
CREATE TABLE pending_promotions (
	app_name TEXT PRIMARY KEY
);

CREATE TABLE workers (
	app_name TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	command TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT ''
);

-- Variables whose value can't be read back, in the panel or anywhere else:
-- they're only passed to the containers. scope is 'project', 'app' or
-- 'worker', owner the project's or app's name. They go on top of the
-- visible variables of the same scope.
CREATE TABLE sealed_vars (
	scope TEXT NOT NULL,
	owner TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (scope, owner, key)
);

-- Docker volumes mounted into an app and its worker.
CREATE TABLE volumes (
	app_name TEXT NOT NULL,
	name TEXT NOT NULL,
	mount_path TEXT NOT NULL,
	PRIMARY KEY (app_name, name)
);

CREATE TABLE databases (
	name TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	db_user TEXT NOT NULL,
	db_password TEXT NOT NULL
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

-- A backup is the files <object_key>/000, /001, ... in bucket of the
-- Cloudflare account account_id (one upload is capped at 300 MB), sealed
-- with file_key (secret.NewFileWriterKey). The check restores it into a
-- scratch database: verified_at '' means not checked yet, verify_error ''
-- means it restored.
CREATE TABLE backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	database TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	bucket TEXT NOT NULL DEFAULT '',
	object_key TEXT NOT NULL,
	parts INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	-- of the whole sealed file, checked before it's restored: the bucket
	-- is outside this server, and a restore runs whatever the dump says
	sha256 TEXT NOT NULL,
	file_key TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	verified_at TEXT NOT NULL DEFAULT '',
	verify_error TEXT NOT NULL DEFAULT '',
	tables INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_backups_database ON backups(database);

-- Backups of apps' volumes: a tar of the volume, sealed like the dumps.
CREATE TABLE volume_backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	volume TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	bucket TEXT NOT NULL DEFAULT '',
	object_key TEXT NOT NULL,
	parts INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL,
	file_key TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	verified_at TEXT NOT NULL DEFAULT '',
	verify_error TEXT NOT NULL DEFAULT '',
	files INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_volume_backups_volume ON volume_backups(app_name, volume);

CREATE TABLE deploy_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	trigger_source TEXT NOT NULL,
	status TEXT NOT NULL,
	output TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Email to the admin when something goes wrong: where it goes, the domain
-- it's sent from (one with Cloudflare Email Routing on) and the name before
-- the @. What hakobu set up in Cloudflare for it, to undo when it's turned
-- off: the address it added and the domain it turned Email Routing on for
-- ('' if they were there already).
CREATE TABLE notify (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	email TEXT NOT NULL,
	sender_domain TEXT NOT NULL,
	sender_name TEXT NOT NULL DEFAULT 'alerts',
	zone_id TEXT NOT NULL DEFAULT '',
	added_address INTEGER NOT NULL DEFAULT 0,
	routed_domain TEXT NOT NULL DEFAULT ''
);

-- The Worker hakobu deployed to the panel's Cloudflare account to watch
-- from outside (ops/watchdog.js), its KV namespace, the targets it was
-- last deployed with (JSON) and the D1 database it records their history
-- in ('' without one).
CREATE TABLE watchdog (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	script TEXT NOT NULL,
	kv_namespace_id TEXT NOT NULL,
	targets TEXT NOT NULL DEFAULT '',
	d1_database_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- The admin turned the watchdog off: hakobu deploys it on its own
-- otherwise, whenever emails are on and the token allows.
CREATE TABLE watchdog_off (
	id INTEGER PRIMARY KEY CHECK (id = 1)
);

-- hakobu is an OAuth authorization server for its MCP endpoint, so Claude
-- and other AI clients can reach the panel on a user's say-so.
--
-- Clients that registered themselves (Dynamic Client Registration). One
-- identified by a metadata document URL instead isn't stored: the document
-- is fetched whenever it asks for access.
CREATE TABLE oauth_clients (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	redirect_uris TEXT NOT NULL, -- one per line
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- A grant is a user's approval of one client; its codes and tokens go
-- with it when it's revoked. github_id is the user who approved it.
CREATE TABLE oauth_grants (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id TEXT NOT NULL,
	client_name TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	scope TEXT NOT NULL,
	github_id INTEGER NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	last_used_at TEXT NOT NULL DEFAULT ''
);

-- Authorization codes, access and refresh tokens, stored by the SHA-256 of
-- the value handed out. A code or refresh token is kept, used_at set, after
-- it's exchanged: presenting it again means someone else has a copy.
CREATE TABLE oauth_tokens (
	id TEXT PRIMARY KEY,
	grant_id INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('code', 'access', 'refresh')),
	code_challenge TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL,
	used_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX oauth_tokens_grant ON oauth_tokens(grant_id);
