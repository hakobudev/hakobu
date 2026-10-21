-- A user's OAuth client of their tailnet (internal/tailscale), which puts
-- their servers in it: keys it mints carry tag.
CREATE TABLE tailscale_clients (
	user_id INTEGER PRIMARY KEY,
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL,
	tag TEXT NOT NULL
);

-- The tailnet device a server is, once it's in its owner's tailnet: its
-- nodeId there, to remove it, and its 100.x address.
ALTER TABLE nodes ADD COLUMN tailnet_device TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN tailnet_ip TEXT NOT NULL DEFAULT '';
