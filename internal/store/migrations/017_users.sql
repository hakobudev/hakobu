-- Who may sign in, known by GitHub's numeric ID (a login can be renamed
-- and then taken by someone else; it's only shown). The first user is the
-- admin: the panel's own settings (updates, its backups, its Cloudflare,
-- notifications, the master key) and invites are theirs. Anyone else joins
-- with an invite and sees only what they own.
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	github_id INTEGER UNIQUE NOT NULL,
	github_login TEXT NOT NULL,
	github_email TEXT NOT NULL DEFAULT '',
	admin INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

INSERT INTO users (github_id, github_login, github_email, admin)
SELECT github_id, github_login, github_email, 1 FROM owner WHERE github_id != 0;

DROP TABLE owner;

-- An invite is a one-time link; secret_hash is the SHA-256 of its secret.
CREATE TABLE invites (
	secret_hash TEXT PRIMARY KEY,
	created_by INTEGER NOT NULL REFERENCES users(id),
	expires_at TEXT NOT NULL
);

-- What a user owns: projects (and with them their apps, databases and
-- storages), servers they added and Cloudflare accounts they connected.
-- 0: nobody's, as made before there were users.
ALTER TABLE projects ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE nodes ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cloudflare_accounts ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;

UPDATE projects SET user_id = (SELECT id FROM users WHERE admin = 1) WHERE EXISTS (SELECT 1 FROM users);
UPDATE nodes SET user_id = (SELECT id FROM users WHERE admin = 1) WHERE EXISTS (SELECT 1 FROM users);
UPDATE cloudflare_accounts SET user_id = (SELECT id FROM users WHERE admin = 1) WHERE EXISTS (SELECT 1 FROM users);
