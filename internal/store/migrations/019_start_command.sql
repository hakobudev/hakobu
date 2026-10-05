-- What an app's container runs instead of its image's own start ('': the
-- image's), as a shell command.
ALTER TABLE apps ADD COLUMN start_command TEXT NOT NULL DEFAULT '';

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage, a.share_volumes, a.memory_mb, a.cpus,
	a.snapshot_db, a.snapshot_at, a.stack, a.start_command
FROM apps a JOIN projects p ON p.id = a.project_id;
