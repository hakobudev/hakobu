-- Projects

-- name: CreateProject :exec
INSERT INTO projects (name) VALUES (?);

-- name: GetProject :one
SELECT * FROM projects WHERE name = ?;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY name;

-- name: CreateUserProject :exec
INSERT INTO projects (name, user_id) VALUES (?, ?);

-- name: ListProjectsOf :many
SELECT * FROM projects WHERE user_id = ? ORDER BY name;

-- name: SetProjectSharedEnv :exec
UPDATE projects SET shared_env = ? WHERE name = ?;

-- name: DeleteProject :exec
DELETE FROM projects WHERE name = ?;

-- Apps

-- name: CreateApp :exec
-- The port is the next free one for the app's proxy, starting at 8081.
INSERT INTO apps (project_id, name, repo, port, container_port, build_path, build_strategy)
VALUES (?, ?, ?, (SELECT MAX(COALESCE(MAX(port), 8080), 8080) + 1 FROM apps), ?, ?, ?);

-- name: GetApp :one
SELECT * FROM app_view WHERE name = ?;

-- name: GetAppByID :one
SELECT * FROM app_view WHERE id = ?;

-- name: GetAppByDomain :one
SELECT * FROM app_view WHERE domain = ? AND domain != '';

-- name: ListApps :many
SELECT * FROM app_view ORDER BY name;

-- name: ListAppsByProject :many
SELECT * FROM app_view WHERE project_id = ? ORDER BY name;

-- name: ListAppsByRepo :many
SELECT * FROM app_view WHERE repo = ?;

-- name: SetAppSettings :exec
UPDATE apps SET container_port = ?, health_check_path = ? WHERE name = ?;

-- name: SetAppDomain :exec
UPDATE apps SET domain = ?, dns_zone_id = ?, dns_record_id = ? WHERE name = ?;

-- name: SetAppBuild :exec
UPDATE apps SET build_path = ?, build_strategy = ?, start_command = ? WHERE name = ?;

-- name: SetAppStack :exec
UPDATE apps SET stack = ? WHERE name = ?;

-- name: SetAppLive :exec
UPDATE apps SET active_slot = ?, live_port = ? WHERE name = ?;

-- name: SetAppEnv :exec
UPDATE apps SET env = ? WHERE name = ?;

-- name: SetAppSentryKey :exec
UPDATE apps SET sentry_key = ? WHERE name = ?;

-- name: SetAppLinkedDB :exec
UPDATE apps SET linked_db = ? WHERE name = ?;

-- name: SetAppLinkedStorage :exec
UPDATE apps SET linked_storage = ? WHERE name = ?;

-- name: AppsUsingDatabase :many
SELECT name FROM apps WHERE linked_db = ? ORDER BY name;

-- name: AppsUsingStorage :many
SELECT name FROM apps WHERE linked_storage = ? ORDER BY name;

-- name: DeleteApp :exec
DELETE FROM apps WHERE name = ?;

-- name: SetAppShareVolumes :exec
UPDATE apps SET share_volumes = ? WHERE name = ?;

-- name: SetAppSnapshot :exec
UPDATE apps SET snapshot_db = ?, snapshot_at = ? WHERE name = ?;

-- name: SetAppLimits :exec
UPDATE apps SET memory_mb = ?, cpus = ? WHERE name = ?;

-- Sealed variables

-- name: SetSealedVar :exec
INSERT INTO sealed_vars (scope, owner, key, value) VALUES (?, ?, ?, ?)
ON CONFLICT(scope, owner, key) DO UPDATE SET value = excluded.value;

-- name: ListSealedVars :many
SELECT * FROM sealed_vars WHERE scope = ? AND owner = ? ORDER BY key;

-- name: ListAllSealedVars :many
SELECT * FROM sealed_vars ORDER BY scope, owner, key;

-- name: DeleteSealedVar :exec
DELETE FROM sealed_vars WHERE scope = ? AND owner = ? AND key = ?;

-- name: DeleteSealedVarsOf :exec
DELETE FROM sealed_vars WHERE scope = ? AND owner = ?;

-- name: DeleteAllSessions :exec
DELETE FROM sessions;

-- Volumes

-- name: AddVolume :exec
INSERT INTO volumes (app_name, name, mount_path) VALUES (?, ?, ?);

-- name: ListVolumes :many
SELECT * FROM volumes WHERE app_name = ? ORDER BY name;

-- name: ListAllVolumes :many
SELECT * FROM volumes;

-- name: DeleteVolume :exec
DELETE FROM volumes WHERE app_name = ? AND name = ?;

-- name: DeleteVolumesOfApp :exec
DELETE FROM volumes WHERE app_name = ?;

-- Workers

-- name: SaveWorker :exec
INSERT INTO workers (app_name, name, command, env) VALUES (?, ?, ?, ?)
ON CONFLICT(app_name) DO UPDATE SET name = excluded.name, command = excluded.command, env = excluded.env;

-- name: GetWorker :one
SELECT * FROM workers WHERE app_name = ?;

-- name: DeleteWorker :exec
DELETE FROM workers WHERE app_name = ?;

-- Databases

-- name: CreateDatabase :exec
INSERT INTO databases (name, project_id, db_user, db_password) VALUES (?, ?, ?, ?);

-- name: GetDatabase :one
SELECT * FROM databases WHERE name = ?;

-- name: ListDatabases :many
SELECT * FROM databases ORDER BY name;

-- name: ListDatabasesByProject :many
SELECT * FROM databases WHERE project_id = ? ORDER BY name;

-- name: SetDatabasePassword :exec
UPDATE databases SET db_password = ? WHERE name = ?;

-- name: DeleteDatabase :exec
DELETE FROM databases WHERE name = ?;

-- Storages

-- name: CreateStorage :exec
INSERT INTO storages (name, project_id, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetStorage :one
SELECT * FROM storages WHERE name = ?;

-- name: ListStoragesByProject :many
SELECT * FROM storages WHERE project_id = ? ORDER BY name;

-- name: SetStorageKeys :exec
UPDATE storages SET access_key_id = ?, secret_access_key = ? WHERE name = ?;

-- name: ListStorages :many
SELECT * FROM storages ORDER BY name;

-- name: DeleteStorage :exec
DELETE FROM storages WHERE name = ?;

-- Backups

-- name: CreateBackup :one
INSERT INTO backups (database, object_key, parts, size_bytes, sha256, file_key, account_id, bucket) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ListBackups :many
SELECT * FROM backups WHERE database = ? ORDER BY id DESC LIMIT ?;

-- name: ListAllBackups :many
SELECT * FROM backups WHERE database = ? ORDER BY id DESC;

-- name: SetBackupVerified :exec
UPDATE backups SET verified_at = ?, verify_error = ?, tables = ? WHERE id = ?;

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = ?;

-- name: DeleteBackupsOf :exec
DELETE FROM backups WHERE database = ?;

-- name: CreateVolumeBackup :one
INSERT INTO volume_backups (app_name, volume, object_key, parts, size_bytes, sha256, file_key, account_id, bucket) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetVolumeBackup :one
SELECT * FROM volume_backups WHERE id = ?;

-- name: ListVolumeBackups :many
SELECT * FROM volume_backups WHERE app_name = ? AND volume = ? ORDER BY id DESC LIMIT ?;

-- name: ListAllVolumeBackups :many
SELECT * FROM volume_backups WHERE app_name = ? AND volume = ? ORDER BY id DESC;

-- name: SetVolumeBackupVerified :exec
UPDATE volume_backups SET verified_at = ?, verify_error = ?, files = ? WHERE id = ?;

-- name: DeleteVolumeBackup :exec
DELETE FROM volume_backups WHERE id = ?;

-- name: DeleteVolumeBackupsOf :exec
DELETE FROM volume_backups WHERE app_name = ? AND volume = ?;

-- name: DeleteVolumeBackupsOfApp :exec
DELETE FROM volume_backups WHERE app_name = ?;

-- GitHub App

-- name: SaveGitHubApp :exec
INSERT OR REPLACE INTO github_app (id, app_id, slug, private_key, webhook_secret, client_id, client_secret)
VALUES (1, ?, ?, ?, ?, ?, ?);

-- name: SetGitHubWebhookSecret :exec
UPDATE github_app SET webhook_secret = ? WHERE id = 1;

-- name: GetGitHubApp :one
SELECT * FROM github_app WHERE id = 1;

-- Users and invites

-- name: GetUserByGitHubID :one
SELECT * FROM users WHERE github_id = ?;

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetAdmin :one
SELECT * FROM users WHERE admin = 1 ORDER BY id LIMIT 1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY id;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (github_id, github_login, admin) VALUES (?, ?, ?) RETURNING id;

-- name: SetUserLogin :exec
UPDATE users SET github_login = ? WHERE id = ?;

-- name: SetUserEmail :exec
UPDATE users SET github_email = ? WHERE id = ?;

-- name: SetUserNotifyEmail :exec
UPDATE users SET notify_email = ? WHERE id = ?;

-- name: SetUserAdmin :exec
UPDATE users SET admin = ? WHERE id = ?;

-- name: CountAdmins :one
SELECT COUNT(*) FROM users WHERE admin = 1;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = ? AND admin = 0;

-- name: DeleteSessionsOf :exec
DELETE FROM sessions WHERE github_id = ?;

-- name: CreateInvite :exec
INSERT INTO invites (secret_hash, created_by, expires_at) VALUES (?, ?, ?);

-- name: TakeInvite :one
DELETE FROM invites WHERE secret_hash = ? AND expires_at > ? RETURNING created_by;

-- name: GetLiveInvite :one
SELECT * FROM invites WHERE secret_hash = ? AND expires_at > ?;

-- name: ListInvites :many
SELECT * FROM invites WHERE expires_at > ? ORDER BY expires_at;

-- name: DeleteInvite :exec
DELETE FROM invites WHERE secret_hash = ?;

-- name: DeleteExpiredInvites :exec
DELETE FROM invites WHERE expires_at <= ?;

-- name: CreateSession :exec
INSERT INTO sessions (id, github_id, signed_in_at, expires_at) VALUES (?, ?, ?, ?);

-- name: GetSessionRow :one
SELECT * FROM sessions WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- Deploy logs

-- name: CreateDeployLog :one
INSERT INTO deploy_logs (app_name, trigger_source, status) VALUES (?, ?, ?) RETURNING id;

-- name: UpdateDeployLog :exec
UPDATE deploy_logs SET status = ?, output = ? WHERE id = ?;

-- name: ListDeployLogs :many
SELECT * FROM deploy_logs WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- ListDeploySummaries is ListDeployLogs without the output, which is
-- large and decrypted on read.
-- name: ListDeploySummaries :many
SELECT id, app_name, trigger_source, status, created_at FROM deploy_logs WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- name: GetDeployLog :one
SELECT * FROM deploy_logs WHERE id = ? AND app_name = ?;

-- name: ListRunningDeployLogs :many
SELECT id, output FROM deploy_logs WHERE status = 'running';

-- name: DeleteDeployLogsOfApp :exec
DELETE FROM deploy_logs WHERE app_name = ?;

-- name: PruneDeployLogs :exec
DELETE FROM deploy_logs WHERE created_at < ?;

-- Cloudflare

-- name: GetCloudflare :one
SELECT * FROM cloudflare WHERE id = 1;

-- name: SaveCloudflareToken :exec
INSERT INTO cloudflare (id, api_token) VALUES (1, ?)
ON CONFLICT(id) DO UPDATE SET api_token = excluded.api_token;

-- name: SetBackupBucket :exec
UPDATE cloudflare SET backup_bucket = ? WHERE id = 1;

-- name: SetTunnelToken :exec
UPDATE cloudflare SET tunnel_token = ? WHERE id = 1;

-- Clients' Cloudflare accounts

-- name: ListCloudflareAccounts :many
SELECT * FROM cloudflare_accounts ORDER BY name;

-- name: CountCloudflareAccountsOf :one
SELECT COUNT(*) FROM cloudflare_accounts WHERE user_id = ?;

-- name: GetCloudflareAccount :one
SELECT * FROM cloudflare_accounts WHERE id = ?;

-- name: GetCloudflareAccountByName :one
SELECT * FROM cloudflare_accounts WHERE name = ?;

-- name: GetCloudflareAccountByAccountID :one
SELECT * FROM cloudflare_accounts WHERE account_id = ?;

-- name: CreateCloudflareAccount :one
INSERT INTO cloudflare_accounts (name, api_token, account_id, user_id) VALUES (?, ?, ?, ?) RETURNING id;

-- name: SetCloudflareAccountToken :exec
UPDATE cloudflare_accounts SET api_token = ? WHERE id = ?;

-- name: SetCloudflareAccountTunnel :exec
UPDATE cloudflare_accounts SET tunnel_id = ?, tunnel_token = ? WHERE id = ?;

-- name: SetCloudflareAccountBackupBucket :exec
UPDATE cloudflare_accounts SET backup_bucket = ? WHERE id = ?;

-- name: DeleteCloudflareAccount :exec
DELETE FROM cloudflare_accounts WHERE id = ?;

-- name: ProjectsInCloudflareAccount :many
SELECT name FROM projects WHERE cloudflare_account_id = ? ORDER BY name;

-- name: SetProjectCloudflareAccount :exec
UPDATE projects SET cloudflare_account_id = ? WHERE id = ?;

-- name: SaveCloudflareTunnel :exec
UPDATE cloudflare SET account_id = ?, tunnel_id = ?, tunnel_token = ?, panel_zone_id = ?, panel_record_id = ? WHERE id = 1;

-- Notifications

-- name: GetNotify :one
SELECT * FROM notify WHERE id = 1;

-- name: SaveNotify :exec
INSERT INTO notify (id, email, sender_name, sender_domain, zone_id, added_address, routed_domain) VALUES (1, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET email = excluded.email, sender_name = excluded.sender_name, sender_domain = excluded.sender_domain,
	zone_id = excluded.zone_id, added_address = excluded.added_address, routed_domain = excluded.routed_domain;

-- name: DeleteNotify :exec
DELETE FROM notify WHERE id = 1;

-- Webhooks

-- name: NoteWebhookDelivery :execrows
-- 0 rows: the delivery was handled before.
INSERT OR IGNORE INTO webhook_deliveries (id) VALUES (?);

-- name: PruneWebhookDeliveries :exec
DELETE FROM webhook_deliveries WHERE created_at < ?;

-- OAuth (MCP clients)

-- name: CreateOAuthClient :exec
INSERT INTO oauth_clients (id, name, redirect_uris) VALUES (?, ?, ?);

-- name: GetOAuthClient :one
SELECT * FROM oauth_clients WHERE id = ?;

-- name: CountOAuthClients :one
SELECT COUNT(*) FROM oauth_clients;

-- name: PruneOAuthClients :exec
DELETE FROM oauth_clients
WHERE oauth_clients.created_at < ? AND oauth_clients.id NOT IN (SELECT client_id FROM oauth_grants);

-- name: CreateOAuthGrant :one
INSERT INTO oauth_grants (client_id, client_name, redirect_uri, scope, github_id) VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: GetOAuthGrant :one
SELECT * FROM oauth_grants WHERE id = ?;

-- Grants with a token still alive, newest first.
-- name: ListOAuthGrants :many
SELECT * FROM oauth_grants g
WHERE EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.grant_id = g.id AND t.kind != 'code' AND t.used_at = '' AND t.expires_at >= ?)
ORDER BY g.id DESC;

-- name: TouchOAuthGrant :exec
UPDATE oauth_grants SET last_used_at = ? WHERE id = ?;

-- name: DeleteOAuthGrant :exec
DELETE FROM oauth_grants WHERE id = ?;

-- name: DeleteAllOAuthGrants :exec
DELETE FROM oauth_grants;

-- name: DeleteOAuthGrantsOf :exec
DELETE FROM oauth_grants WHERE github_id = ?;

-- Grants none of whose tokens can be used any more.
-- name: PruneOAuthGrants :exec
DELETE FROM oauth_grants
WHERE NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.grant_id = oauth_grants.id AND t.expires_at >= ?);

-- name: CreateOAuthToken :exec
INSERT INTO oauth_tokens (id, grant_id, kind, code_challenge, expires_at) VALUES (?, ?, ?, ?, ?);

-- name: GetOAuthToken :one
SELECT * FROM oauth_tokens WHERE id = ?;

-- Marks a code or refresh token exchanged; 0 rows if it was already.
-- name: UseOAuthToken :execrows
UPDATE oauth_tokens SET used_at = ? WHERE id = ? AND used_at = '';

-- name: PruneOAuthTokens :exec
DELETE FROM oauth_tokens WHERE expires_at < ?;


-- Watchdog

-- name: GetWatchdog :one
SELECT * FROM watchdog WHERE id = 1;

-- name: SaveWatchdog :exec
INSERT INTO watchdog (id, script, kv_namespace_id, targets, d1_database_id) VALUES (1, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET script = excluded.script, kv_namespace_id = excluded.kv_namespace_id,
	targets = excluded.targets, d1_database_id = excluded.d1_database_id;

-- name: DeleteWatchdog :exec
DELETE FROM watchdog;

-- name: WatchdogTurnedOff :one
SELECT EXISTS (SELECT 1 FROM watchdog_off);

-- name: TurnWatchdogOff :exec
INSERT OR IGNORE INTO watchdog_off (id) VALUES (1);

-- name: AllowWatchdog :exec
DELETE FROM watchdog_off;

-- Nodes

-- name: ListNodes :many
SELECT * FROM nodes ORDER BY name;

-- name: CountNodesOf :one
SELECT COUNT(*) FROM nodes WHERE user_id = ?;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE name = ?;

-- name: GetNodeByKey :one
SELECT * FROM nodes WHERE public_key = ? AND public_key != '';

-- name: GetNodeByJoinSecret :one
SELECT * FROM nodes WHERE join_secret_hash = ? AND join_secret_hash != '';

-- name: CreateNode :exec
INSERT INTO nodes (name, join_secret_hash, join_expires, user_id) VALUES (?, ?, ?, ?);

-- name: SetNodeJoin :exec
UPDATE nodes SET join_secret_hash = ?, join_expires = ? WHERE name = ?;

-- name: JoinNode :exec
UPDATE nodes SET public_key = ?, join_secret_hash = '', join_expires = '' WHERE id = ?;

-- name: SeeNode :exec
UPDATE nodes SET version = ?, last_seen = ? WHERE id = ?;

-- name: DeleteNode :exec
DELETE FROM nodes WHERE name = ?;

-- name: GetLinkKey :one
SELECT private_key FROM link_key WHERE id = 1;

-- name: SaveLinkKey :exec
INSERT INTO link_key (id, private_key) VALUES (1, ?);

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ProjectsOnNode :many
SELECT name FROM projects WHERE node_id = ? ORDER BY name;

-- name: SetProjectNode :exec
UPDATE projects SET node_id = ? WHERE id = ?;

-- Server tunnels

-- name: ListServerTunnels :many
SELECT t.*, n.name AS node_name FROM server_tunnels t JOIN nodes n ON n.id = t.node_id ORDER BY t.id;

-- name: GetServerTunnel :one
SELECT * FROM server_tunnels WHERE node_id = ? AND cloudflare_account_id = ?;

-- name: CreateServerTunnel :exec
INSERT INTO server_tunnels (node_id, cloudflare_account_id, tunnel_id, tunnel_token) VALUES (?, ?, ?, ?);

-- name: SetServerTunnelToken :exec
UPDATE server_tunnels SET tunnel_token = ? WHERE id = ?;

-- name: DeleteServerTunnel :exec
DELETE FROM server_tunnels WHERE id = ?;

-- name: AddPendingPromotion :exec
INSERT OR IGNORE INTO pending_promotions (app_name) VALUES (?);

-- name: ListPendingPromotions :many
SELECT app_name FROM pending_promotions ORDER BY app_name;

-- name: DeletePendingPromotion :exec
DELETE FROM pending_promotions WHERE app_name = ?;
