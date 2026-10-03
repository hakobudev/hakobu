-- Servers other than the panel's that run projects (internal/node,
-- internal/link). A node joins with a one-time token: join_secret_hash is
-- the SHA-256 of its secret until then, '' after; public_key is the
-- node's ed25519 key (hex) from then on.
CREATE TABLE nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
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
