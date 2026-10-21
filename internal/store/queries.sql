-- Projects

-- name: CreateProject :one
INSERT INTO projects (name, user_id) VALUES (?, ?) RETURNING id;

-- name: GetProject :one
-- A project by its name among its owner's.
SELECT * FROM projects WHERE user_id = ? AND name = ?;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY name, id;

-- name: ListProjectsOf :many
SELECT * FROM projects WHERE user_id = ? ORDER BY name;

-- name: RenameProject :exec
UPDATE projects SET name = ? WHERE id = ?;

-- name: SetProjectSharedEnv :exec
UPDATE projects SET shared_env = ? WHERE id = ?;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = ?;

-- Apps

-- name: CreateApp :one
-- The port is the next free one for the app's proxy, starting at 8081.
INSERT INTO apps (project_id, name, repo, port, container_port, build_path, build_strategy)
VALUES (?, ?, ?, (SELECT MAX(COALESCE(MAX(port), 8080), 8080) + 1 FROM apps), ?, ?, ?) RETURNING id;

-- name: GetApp :one
-- An app by its name in its project.
SELECT * FROM app_view WHERE project_id = ? AND name = ?;

-- name: GetAppByID :one
SELECT * FROM app_view WHERE id = ?;

-- name: GetAppByDomain :one
SELECT * FROM app_view WHERE domain = ? AND domain != '';

-- name: ListApps :many
SELECT * FROM app_view ORDER BY project_name, name;

-- name: ListAppsByProject :many
SELECT * FROM app_view WHERE project_id = ? ORDER BY name;

-- name: ListAppsByRepo :many
SELECT * FROM app_view WHERE repo = ?;

-- name: SetAppSettings :exec
UPDATE apps SET container_port = ?, health_check_path = ? WHERE id = ?;

-- name: SetAppDomain :exec
UPDATE apps SET domain = ?, dns_zone_id = ?, dns_record_id = ? WHERE id = ?;

-- name: SetAppBuild :exec
UPDATE apps SET build_path = ?, build_strategy = ?, start_command = ? WHERE id = ?;

-- name: SetAppStack :exec
UPDATE apps SET stack = ? WHERE id = ?;

-- name: SetAppStopped :exec
UPDATE apps SET stopped = ? WHERE id = ?;

-- name: SetAppLive :exec
UPDATE apps SET active_slot = ?, live_port = ? WHERE id = ?;

-- name: SetAppEnv :exec
UPDATE apps SET env = ? WHERE id = ?;

-- name: SetAppSentryKey :exec
UPDATE apps SET sentry_key = ? WHERE id = ?;

-- name: SetAppLinkedDB :exec
UPDATE apps SET linked_db_id = ? WHERE id = ?;

-- name: SetAppLinkedStorage :exec
UPDATE apps SET linked_storage_id = ? WHERE id = ?;

-- name: AppsUsingDatabase :many
SELECT name FROM apps WHERE linked_db_id = ? ORDER BY name;

-- name: AppsUsingStorage :many
SELECT name FROM apps WHERE linked_storage_id = ? ORDER BY name;

-- name: SetAppLinkedCache :exec
UPDATE apps SET linked_cache_id = ? WHERE id = ?;

-- name: AppsUsingCache :many
SELECT name FROM apps WHERE linked_cache_id = ? ORDER BY name;

-- name: RenameApp :exec
UPDATE apps SET name = ? WHERE id = ?;

-- name: DeleteApp :exec
DELETE FROM apps WHERE id = ?;

-- name: SetAppShareVolumes :exec
UPDATE apps SET share_volumes = ? WHERE id = ?;

-- name: SetAppSnapshot :exec
UPDATE apps SET snapshot_db_id = ?, snapshot_at = ? WHERE id = ?;

-- name: SetAppLimits :exec
UPDATE apps SET memory_mb = ?, cpus = ? WHERE id = ?;

-- Sealed variables

-- name: SetSealedVar :exec
INSERT INTO sealed_vars (scope, owner_id, key, value) VALUES (?, ?, ?, ?)
ON CONFLICT(scope, owner_id, key) DO UPDATE SET value = excluded.value;

-- name: ListSealedVars :many
SELECT * FROM sealed_vars WHERE scope = ? AND owner_id = ? ORDER BY key;

-- name: ListAllSealedVars :many
SELECT * FROM sealed_vars ORDER BY scope, owner_id, key;

-- name: DeleteSealedVar :exec
DELETE FROM sealed_vars WHERE scope = ? AND owner_id = ? AND key = ?;

-- name: DeleteSealedVarsOf :exec
DELETE FROM sealed_vars WHERE scope = ? AND owner_id = ?;

-- name: DeleteAllSessions :exec
DELETE FROM sessions;

-- Volumes

-- name: AddVolume :exec
INSERT INTO volumes (app_id, name, mount_path) VALUES (?, ?, ?);

-- name: ListVolumes :many
SELECT * FROM volumes WHERE app_id = ? ORDER BY name;

-- name: SetAppRoute :exec
INSERT INTO app_routes (app_id, path, target_id) VALUES (?, ?, ?)
ON CONFLICT(app_id, path) DO UPDATE SET target_id = excluded.target_id;

-- name: DeleteAppRoute :exec
DELETE FROM app_routes WHERE app_id = ? AND path = ?;

-- name: ListAppRoutes :many
SELECT * FROM app_routes WHERE app_id = ? ORDER BY path;

-- name: ListAllAppRoutes :many
SELECT * FROM app_routes ORDER BY app_id, path;

-- name: DeleteRoutesOfApp :exec
DELETE FROM app_routes WHERE app_id = ?1 OR target_id = ?1;

-- name: ListAllVolumes :many
SELECT * FROM volumes;

-- name: DeleteVolume :exec
DELETE FROM volumes WHERE app_id = ? AND name = ?;

-- name: DeleteVolumesOfApp :exec
DELETE FROM volumes WHERE app_id = ?;

-- Workers

-- name: SaveWorker :exec
INSERT INTO workers (app_id, name, command, env) VALUES (?, ?, ?, ?)
ON CONFLICT(app_id) DO UPDATE SET name = excluded.name, command = excluded.command, env = excluded.env;

-- name: GetWorker :one
SELECT * FROM workers WHERE app_id = ?;

-- name: DeleteWorker :exec
DELETE FROM workers WHERE app_id = ?;

-- Databases

-- name: CreateDatabase :one
INSERT INTO databases (name, project_id, db_password) VALUES (?, ?, ?) RETURNING id;

-- name: GetDatabase :one
-- A database by its name in its project.
SELECT * FROM databases WHERE project_id = ? AND name = ?;

-- name: GetDatabaseByID :one
SELECT * FROM databases WHERE id = ?;

-- name: ListDatabases :many
SELECT * FROM databases ORDER BY id;

-- name: ListDatabasesByProject :many
SELECT * FROM databases WHERE project_id = ? ORDER BY name;

-- name: SetDatabasePassword :exec
UPDATE databases SET db_password = ? WHERE id = ?;

-- name: DeleteDatabase :exec
DELETE FROM databases WHERE id = ?;

-- Storages

-- name: CreateStorage :one
INSERT INTO storages (name, project_id, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetStorage :one
-- A storage by its name in its project.
SELECT * FROM storages WHERE project_id = ? AND name = ?;

-- name: GetStorageByID :one
SELECT * FROM storages WHERE id = ?;

-- name: ListStoragesByProject :many
SELECT * FROM storages WHERE project_id = ? ORDER BY name;

-- name: SetStorageKeys :exec
UPDATE storages SET access_key_id = ?, secret_access_key = ? WHERE id = ?;

-- name: ListStorages :many
SELECT * FROM storages ORDER BY id;

-- name: DeleteStorage :exec
DELETE FROM storages WHERE id = ?;

-- Caches

-- name: CreateCache :one
INSERT INTO caches (name, project_id, password, memory_mb) VALUES (?, ?, ?, ?) RETURNING id;

-- name: GetCache :one
-- A cache by its name in its project.
SELECT * FROM caches WHERE project_id = ? AND name = ?;

-- name: GetCacheByID :one
SELECT * FROM caches WHERE id = ?;

-- name: ListCachesByProject :many
SELECT * FROM caches WHERE project_id = ? ORDER BY name;

-- name: ListCaches :many
SELECT * FROM caches ORDER BY id;

-- name: SetCacheMemory :exec
UPDATE caches SET memory_mb = ? WHERE id = ?;

-- name: SetCachePassword :exec
UPDATE caches SET password = ? WHERE id = ?;

-- name: DeleteCache :exec
DELETE FROM caches WHERE id = ?;

-- Backups

-- name: CreateBackup :one
INSERT INTO backups (database_id, object_key, parts, size_bytes, sha256, key_fingerprint, account_id, bucket) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ListBackups :many
SELECT * FROM backups WHERE database_id = ? ORDER BY id DESC LIMIT ?;

-- name: ListAllBackups :many
SELECT * FROM backups WHERE database_id = ? ORDER BY id DESC;

-- name: SetBackupVerified :exec
UPDATE backups SET verified_at = ?, verify_error = ?, tables = ? WHERE id = ?;

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = ?;

-- name: DeleteBackupsOf :exec
DELETE FROM backups WHERE database_id = ?;

-- name: CreateVolumeBackup :one
INSERT INTO volume_backups (app_id, volume, object_key, parts, size_bytes, sha256, key_fingerprint, account_id, bucket) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: GetVolumeBackup :one
SELECT * FROM volume_backups WHERE id = ?;

-- name: ListVolumeBackups :many
SELECT * FROM volume_backups WHERE app_id = ? AND volume = ? ORDER BY id DESC LIMIT ?;

-- name: ListAllVolumeBackups :many
SELECT * FROM volume_backups WHERE app_id = ? AND volume = ? ORDER BY id DESC;

-- name: SetVolumeBackupVerified :exec
UPDATE volume_backups SET verified_at = ?, verify_error = ?, files = ? WHERE id = ?;

-- name: DeleteVolumeBackup :exec
DELETE FROM volume_backups WHERE id = ?;

-- name: DeleteVolumeBackupsOf :exec
DELETE FROM volume_backups WHERE app_id = ? AND volume = ?;

-- name: DeleteVolumeBackupsOfApp :exec
DELETE FROM volume_backups WHERE app_id = ?;

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

-- name: SetUserSharedDomain :exec
UPDATE users SET shared_domain = ? WHERE id = ?;

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
INSERT INTO deploy_logs (app_id, trigger_source, status) VALUES (?, ?, ?) RETURNING id;

-- name: UpdateDeployLog :exec
UPDATE deploy_logs SET status = ?, output = ? WHERE id = ?;

-- name: ListDeployLogs :many
SELECT * FROM deploy_logs WHERE app_id = ? ORDER BY id DESC LIMIT ?;

-- ListDeploySummaries is ListDeployLogs without the output, which is
-- large and decrypted on read.
-- name: ListDeploySummaries :many
SELECT id, app_id, trigger_source, status, created_at FROM deploy_logs WHERE app_id = ? ORDER BY id DESC LIMIT ?;

-- name: GetDeployLog :one
SELECT * FROM deploy_logs WHERE id = ? AND app_id = ?;

-- name: ListRunningDeployLogs :many
SELECT id, output FROM deploy_logs WHERE status = 'running';

-- name: DeleteDeployLogsOfApp :exec
DELETE FROM deploy_logs WHERE app_id = ?;

-- name: PruneDeployLogs :exec
DELETE FROM deploy_logs WHERE created_at < ?;

-- Cloudflare

-- name: GetCloudflare :one
SELECT * FROM cloudflare WHERE id = 1;

-- name: SaveCloudflareToken :exec
INSERT INTO cloudflare (id, api_token) VALUES (1, ?)
ON CONFLICT(id) DO UPDATE SET api_token = excluded.api_token;

-- name: SetCloudflareOwner :exec
UPDATE cloudflare SET user_id = ?, token_group = ? WHERE id = 1;

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
INSERT INTO cloudflare_accounts (name, user_id, api_token, token_group, account_id, title, domains) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: SetCloudflareAccountInfo :exec
UPDATE cloudflare_accounts SET title = ?, domains = ? WHERE id = ?;

-- name: SetTokenGroupToken :exec
UPDATE cloudflare_accounts SET api_token = ? WHERE token_group = ?;

-- name: SetCloudflareAccountTunnel :exec
UPDATE cloudflare_accounts SET tunnel_id = ?, tunnel_token = ? WHERE id = ?;

-- name: SetCloudflareAccountBackupBucket :exec
UPDATE cloudflare_accounts SET backup_bucket = ? WHERE id = ?;

-- name: DeleteCloudflareAccount :exec
DELETE FROM cloudflare_accounts WHERE id = ?;

-- name: ProjectsInCloudflareAccount :many
SELECT name FROM projects WHERE cloudflare_account_id = ? ORDER BY name;

-- name: UserProjectsInPanelAccount :many
SELECT name FROM projects WHERE cloudflare_account_id IS NULL AND user_id = ? ORDER BY name;

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

-- Alerts from clients' accounts

-- name: GetAccountAlerts :one
SELECT * FROM account_alerts WHERE cloudflare_account_id = ?;

-- name: ListAccountAlerts :many
SELECT * FROM account_alerts ORDER BY cloudflare_account_id;

-- name: SaveAccountEmail :exec
INSERT INTO account_alerts (cloudflare_account_id, email, added_address, sender_domain, zone_id, routed_domain) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (cloudflare_account_id) DO UPDATE SET email = excluded.email, added_address = excluded.added_address,
	sender_domain = excluded.sender_domain, zone_id = excluded.zone_id, routed_domain = excluded.routed_domain;

-- name: SaveAccountWatchdog :exec
UPDATE account_alerts SET script = ?, kv_namespace_id = ?, targets = ?, d1_database_id = ? WHERE cloudflare_account_id = ?;

-- name: DeleteAccountAlerts :exec
DELETE FROM account_alerts WHERE cloudflare_account_id = ?;

-- name: ProjectIDsInCloudflareAccount :many
SELECT id FROM projects WHERE cloudflare_account_id = ?;

-- Nodes

-- name: ListNodes :many
SELECT * FROM nodes ORDER BY name, id;

-- name: CountNodesOf :one
SELECT COUNT(*) FROM nodes WHERE user_id = ?;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE user_id = ? AND name = ?;

-- name: GetNodeByKey :one
SELECT * FROM nodes WHERE public_key = ? AND public_key != '';

-- name: GetNodeByJoinSecret :one
SELECT * FROM nodes WHERE join_secret_hash = ? AND join_secret_hash != '';

-- name: CreateNode :exec
INSERT INTO nodes (name, join_secret_hash, join_expires, user_id) VALUES (?, ?, ?, ?);

-- name: SetNodeJoin :exec
UPDATE nodes SET join_secret_hash = ?, join_expires = ? WHERE id = ?;

-- name: JoinNode :exec
UPDATE nodes SET public_key = ?, join_secret_hash = '', join_expires = '' WHERE id = ?;

-- name: SeeNode :exec
UPDATE nodes SET version = ?, last_seen = ? WHERE id = ?;

-- name: RenameNode :exec
UPDATE nodes SET name = ? WHERE id = ?;

-- name: DeleteNode :exec
DELETE FROM nodes WHERE id = ?;

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
INSERT OR IGNORE INTO pending_promotions (app_id) VALUES (?);

-- name: ListPendingPromotions :many
SELECT app_id FROM pending_promotions ORDER BY app_id;

-- name: DeletePendingPromotion :exec
DELETE FROM pending_promotions WHERE app_id = ?;

-- name: ListTailnets :many
SELECT * FROM tailscale_tailnets WHERE user_id = ? ORDER BY name;

-- name: GetTailnet :one
SELECT * FROM tailscale_tailnets WHERE id = ?;

-- name: GetTailnetByName :one
SELECT * FROM tailscale_tailnets WHERE user_id = ? AND name = ?;

-- name: CreateTailnet :one
INSERT INTO tailscale_tailnets (user_id, name, client_id, client_secret, tag) VALUES (?, ?, ?, ?, ?) RETURNING id;

-- name: UpdateTailnetClient :exec
UPDATE tailscale_tailnets SET client_id = ?, client_secret = ?, tag = ? WHERE id = ?;

-- name: RenameTailnet :exec
UPDATE tailscale_tailnets SET name = ? WHERE id = ?;

-- name: DeleteTailnet :exec
DELETE FROM tailscale_tailnets WHERE id = ?;

-- name: SetNodeTailnet :exec
UPDATE nodes SET tailnet_id = ?, tailnet_device = ?, tailnet_ip = ? WHERE id = ?;
