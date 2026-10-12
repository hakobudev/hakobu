-- An app its owner stopped stays stopped: through restarts of hakobu and
-- its server, and pushes to its repo, until it's started or deployed.
ALTER TABLE apps ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, p.user_id, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db_id, a.linked_storage_id, a.linked_cache_id, a.share_volumes, a.memory_mb, a.cpus,
	a.snapshot_db_id, a.snapshot_at, a.stack, a.start_command, a.stopped
FROM apps a JOIN projects p ON p.id = a.project_id;
