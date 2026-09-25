-- The Cloudflare account connected through OAuth, the tunnel hakobu created
-- in it, and the DNS records hakobu owns for the panel and each app.
CREATE TABLE cloudflare (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	access_token TEXT NOT NULL,
	refresh_token TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	tunnel_id TEXT NOT NULL DEFAULT '',
	tunnel_token TEXT NOT NULL DEFAULT '',
	panel_zone_id TEXT NOT NULL DEFAULT '',
	panel_record_id TEXT NOT NULL DEFAULT ''
);

ALTER TABLE apps ADD COLUMN dns_zone_id TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN dns_record_id TEXT NOT NULL DEFAULT '';

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage
FROM apps a JOIN projects p ON p.id = a.project_id;
