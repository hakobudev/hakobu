-- The server a project runs on: NULL is the panel's own.
ALTER TABLE projects ADD COLUMN node_id INTEGER REFERENCES nodes(id);

-- Tunnels on servers other than the panel's: one per Cloudflare account
-- with projects there (cloudflare_account_id 0 is the panel's account), as
-- one tunnel can't reach two servers. The panel's server's tunnels stay in
-- the cloudflare and cloudflare_accounts tables.
CREATE TABLE server_tunnels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_id INTEGER NOT NULL REFERENCES nodes(id),
	cloudflare_account_id INTEGER NOT NULL DEFAULT 0,
	tunnel_id TEXT NOT NULL,
	tunnel_token TEXT NOT NULL,
	UNIQUE (node_id, cloudflare_account_id)
);
