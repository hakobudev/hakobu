-- A user may connect several tailnets (internal/tailscale), as with
-- Cloudflare accounts: theirs and a client's, say. name is theirs for it.
-- Each server remembers which one it's in (nodes.tailnet_id; NULL while
-- hakobu hasn't found it by its address).
CREATE TABLE tailscale_tailnets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL,
	tag TEXT NOT NULL,
	UNIQUE (user_id, name)
);
INSERT INTO tailscale_tailnets (user_id, name, client_id, client_secret, tag)
	SELECT user_id, 'tailnet', client_id, client_secret, tag FROM tailscale_clients;
DROP TABLE tailscale_clients;

ALTER TABLE nodes ADD COLUMN tailnet_id INTEGER REFERENCES tailscale_tailnets(id) ON DELETE SET NULL;
