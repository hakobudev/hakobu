-- Apps are published through the Cloudflare Tunnel by domain; the
-- Tailscale Funnel port is gone.
DROP VIEW app_view;
ALTER TABLE apps DROP COLUMN funnel_port;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.port, a.container_port,
	a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage
FROM apps a JOIN projects p ON p.id = a.project_id;
