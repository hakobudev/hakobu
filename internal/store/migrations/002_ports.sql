-- container_port 0 now means "detect": live_port is the port the running
-- container was found listening on. funnel_port is the Tailscale Funnel
-- port (8443 or 10000) the app is published on, 0 if none.
ALTER TABLE apps ADD COLUMN live_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN funnel_port INTEGER NOT NULL DEFAULT 0;

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.port, a.container_port,
	a.live_port, a.funnel_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage
FROM apps a JOIN projects p ON p.id = a.project_id;
