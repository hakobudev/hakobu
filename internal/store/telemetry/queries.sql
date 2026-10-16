-- name: CreateTelemetryEvent :exec
INSERT INTO telemetry_events (app_id, kind, level, message, payload, trace_id, fingerprint) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListTelemetryEvents :many
SELECT * FROM telemetry_events WHERE app_id = ? ORDER BY id DESC LIMIT ?;

-- name: GetTelemetryEvent :one
SELECT * FROM telemetry_events WHERE id = ? AND app_id = ?;

-- name: DeleteTelemetryOfApp :exec
DELETE FROM telemetry_events WHERE app_id = ?;

-- name: DeleteResolvedIssuesOfApp :exec
DELETE FROM resolved_issues WHERE app_id = ?;

-- name: LastTelemetryOfKind :one
SELECT created_at FROM telemetry_events WHERE app_id = ? AND kind = ? ORDER BY id DESC LIMIT 1;

-- name: PruneLogs :exec
DELETE FROM telemetry_events WHERE kind = 'log' AND created_at < ?;

-- name: PruneEvents :exec
DELETE FROM telemetry_events WHERE created_at < ?;

-- name: PruneResolvedIssues :exec
DELETE FROM resolved_issues WHERE resolved_at < ?;

-- name: PutSample :exec
INSERT INTO samples (target, res, slot, ts, cpu, cpu_max, cpu_limit, mem, mem_max, mem_limit, net_rx, net_tx, disk_read, disk_write, load, disk_used, disk_total)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target, res, slot) DO UPDATE SET
	ts = excluded.ts, cpu = excluded.cpu, cpu_max = excluded.cpu_max, cpu_limit = excluded.cpu_limit,
	mem = excluded.mem, mem_max = excluded.mem_max, mem_limit = excluded.mem_limit,
	net_rx = excluded.net_rx, net_tx = excluded.net_tx, disk_read = excluded.disk_read, disk_write = excluded.disk_write,
	load = excluded.load, disk_used = excluded.disk_used, disk_total = excluded.disk_total;

-- name: SummarizeSamples :many
SELECT target,
	CAST(avg(cpu) AS REAL) AS cpu, CAST(max(cpu_max) AS REAL) AS cpu_max, CAST(max(cpu_limit) AS REAL) AS cpu_limit,
	CAST(avg(mem) AS INTEGER) AS mem, CAST(max(mem_max) AS INTEGER) AS mem_max, CAST(max(mem_limit) AS INTEGER) AS mem_limit,
	CAST(avg(net_rx) AS REAL) AS net_rx, CAST(avg(net_tx) AS REAL) AS net_tx,
	CAST(avg(disk_read) AS REAL) AS disk_read, CAST(avg(disk_write) AS REAL) AS disk_write,
	CAST(avg(load) AS REAL) AS load, CAST(max(disk_used) AS INTEGER) AS disk_used, CAST(max(disk_total) AS INTEGER) AS disk_total
FROM samples WHERE res = ? AND ts >= ? AND ts < ? GROUP BY target;

-- name: ListSamples :many
SELECT * FROM samples WHERE target = ? AND res = ? AND ts >= ? ORDER BY ts;

-- name: LatestSamples :many
SELECT * FROM samples WHERE res = ? AND ts >= ? ORDER BY ts;

-- name: DeleteSamplesOf :exec
DELETE FROM samples WHERE target = ?;

-- name: PruneSamples :exec
DELETE FROM samples WHERE ts < ?;

-- name: CountTraceRoutes :one
SELECT count(*) FROM trace_routes WHERE app_id = ? AND hour = ?;

-- name: HasTraceRoute :one
SELECT EXISTS (SELECT 1 FROM trace_routes WHERE app_id = ? AND hour = ? AND name = ?);

-- name: CountTrace :exec
INSERT INTO trace_routes (app_id, name, hour, count, errors, total_ms, b0, b1, b2, b3, b4, b5, b6, b7, b8, b9, b10)
VALUES (@app_id, @name, @hour, 1, @errors, @ms,
	@bucket = 0, @bucket = 1, @bucket = 2, @bucket = 3, @bucket = 4, @bucket = 5,
	@bucket = 6, @bucket = 7, @bucket = 8, @bucket = 9, @bucket = 10)
ON CONFLICT (app_id, hour, name) DO UPDATE SET
	count = count + 1, errors = errors + excluded.errors, total_ms = total_ms + excluded.total_ms,
	b0 = b0 + excluded.b0, b1 = b1 + excluded.b1, b2 = b2 + excluded.b2, b3 = b3 + excluded.b3,
	b4 = b4 + excluded.b4, b5 = b5 + excluded.b5, b6 = b6 + excluded.b6, b7 = b7 + excluded.b7,
	b8 = b8 + excluded.b8, b9 = b9 + excluded.b9, b10 = b10 + excluded.b10;

-- name: ListTraceRoutes :many
SELECT * FROM trace_routes WHERE app_id = ? AND hour >= ?;

-- name: CreateTrace :exec
INSERT INTO traces (app_id, trace_id, name, status, http_status, duration_ms, slow_span, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTraces :many
SELECT id, app_id, trace_id, name, status, http_status, duration_ms, slow_span, created_at
FROM traces WHERE app_id = ? ORDER BY id DESC LIMIT ?;

-- name: GetTrace :one
SELECT * FROM traces WHERE id = ? AND app_id = ?;

-- name: TraceByTraceID :one
SELECT id FROM traces WHERE app_id = ? AND trace_id = ? ORDER BY id DESC LIMIT 1;

-- name: DeleteTracesOfApp :exec
DELETE FROM traces WHERE app_id = ?;

-- name: DeleteTraceRoutesOfApp :exec
DELETE FROM trace_routes WHERE app_id = ?;

-- name: PruneTraces :exec
DELETE FROM traces WHERE created_at < ?;

-- name: PruneTraceRoutes :exec
DELETE FROM trace_routes WHERE hour < ?;

-- name: ListProblems :many
SELECT * FROM telemetry_events WHERE app_id = ? AND kind != 'log' ORDER BY id DESC LIMIT ?;

-- name: ListIssues :many
-- An app's issues, each as its latest event, newest first: with the
-- resolved ones, or only the open ones (whose resolved_at is empty).
SELECT e.id, e.kind, e.level, e.message, e.trace_id, e.fingerprint,
	g.count, CAST(g.first_at AS TEXT) AS first_at, e.created_at AS last_at,
	CAST(CASE WHEN r.through_id >= e.id THEN r.resolved_at ELSE '' END AS TEXT) AS resolved_at
FROM (
	SELECT t.fingerprint, max(t.id) AS last_id, count(*) AS count, min(t.created_at) AS first_at
	FROM telemetry_events t WHERE t.app_id = @app_id AND t.kind != 'log' GROUP BY t.fingerprint
) g
JOIN telemetry_events e ON e.id = g.last_id
LEFT JOIN resolved_issues r ON r.app_id = @app_id AND r.fingerprint = g.fingerprint
WHERE CAST(@with_resolved AS BOOLEAN) OR r.through_id IS NULL OR r.through_id < e.id
ORDER BY e.id DESC LIMIT @lim;

-- name: ResolveIssue :execrows
-- Resolves the issue of event id, up to its latest event.
INSERT INTO resolved_issues (app_id, fingerprint, through_id)
SELECT e.app_id, e.fingerprint, (SELECT max(l.id) FROM telemetry_events l WHERE l.app_id = e.app_id AND l.fingerprint = e.fingerprint)
FROM telemetry_events e WHERE e.id = @id AND e.app_id = @app_id AND e.kind != 'log'
ON CONFLICT (app_id, fingerprint) DO UPDATE SET through_id = excluded.through_id, resolved_at = excluded.resolved_at;

-- name: ListUnfingerprinted :many
-- Events from before issues, which migration 005 marked 'event:<id>'.
SELECT id, kind, level, payload FROM telemetry_events
WHERE fingerprint LIKE 'event:%' AND id > ? ORDER BY id LIMIT ?;

-- name: SetFingerprint :exec
UPDATE telemetry_events SET fingerprint = ? WHERE id = ?;

-- name: DeleteUnfingerprintedResolved :exec
DELETE FROM resolved_issues WHERE fingerprint LIKE 'event:%';

-- name: ReopenIssue :execrows
DELETE FROM resolved_issues WHERE resolved_issues.app_id = @app_id
	AND resolved_issues.fingerprint = (SELECT t.fingerprint FROM telemetry_events t WHERE t.id = @id AND t.app_id = @app_id);

-- name: AddPanelLog :exec
INSERT INTO panel_log (level, message) VALUES (?, ?);

-- name: ListPanelLog :many
SELECT * FROM panel_log WHERE level >= ? ORDER BY id DESC LIMIT ?;

-- name: CountPanelErrorsSince :one
SELECT count(*) FROM panel_log WHERE level >= 2 AND created_at >= ?;

-- name: PrunePanelLog :exec
DELETE FROM panel_log WHERE created_at < ?;

-- name: PutDataCheck :exec
INSERT OR REPLACE INTO data_checks (target, ts, up, why, size) VALUES (?, ?, ?, ?, ?);

-- name: DataSizeAt :one
SELECT ts, size FROM data_checks WHERE target = ? AND ts <= ? AND size > 0 ORDER BY ts DESC LIMIT 1;

-- name: DataCheckWindow :one
SELECT count(*) AS n, CAST(coalesce(sum(up), 0) AS INTEGER) AS up FROM data_checks WHERE target = ? AND ts >= ?;

-- name: DataChecksDown :many
SELECT ts, why FROM data_checks WHERE target = ? AND up = 0 AND ts >= ? ORDER BY ts DESC LIMIT 5000;

-- name: LastDataCheck :one
SELECT CAST(coalesce(max(ts), 0) AS INTEGER) FROM data_checks WHERE target = ?;

-- name: DataChecksByDay :many
SELECT CAST(ts / 86400 AS INTEGER) AS day, count(*) AS n, CAST(coalesce(sum(up), 0) AS INTEGER) AS up
FROM data_checks WHERE target = ? AND ts >= ? GROUP BY day;

-- name: DeleteDataChecksOf :exec
DELETE FROM data_checks WHERE target = ?;

-- name: PruneDataChecks :exec
DELETE FROM data_checks WHERE ts < ?;
