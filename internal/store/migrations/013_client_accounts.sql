-- Cloudflare accounts of the owner's clients. A project in one of them
-- gets its domains, storages and backups there, through a tunnel of its
-- own; projects without one use the panel's account (table cloudflare).
-- api_token is the client's account-owned token; backup_bucket '' until
-- the account's first backup.
CREATE TABLE cloudflare_accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	api_token TEXT NOT NULL,
	account_id TEXT UNIQUE NOT NULL,
	tunnel_id TEXT NOT NULL DEFAULT '',
	tunnel_token TEXT NOT NULL DEFAULT '',
	backup_bucket TEXT NOT NULL DEFAULT ''
);

ALTER TABLE projects ADD COLUMN cloudflare_account_id INTEGER REFERENCES cloudflare_accounts(id);

-- Where each backup is: the Cloudflare account and bucket it went to.
-- '' for backups made before, all in the panel's backup bucket.
ALTER TABLE backups ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN bucket TEXT NOT NULL DEFAULT '';
ALTER TABLE volume_backups ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE volume_backups ADD COLUMN bucket TEXT NOT NULL DEFAULT '';
