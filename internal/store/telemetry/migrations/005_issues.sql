-- Errors, crashes, out-of-memory kills and health checks group into issues
-- by fingerprint (internal/ingest.Fingerprint): a hash, so it holds none of
-- the message. Events from before have none to go by and stand alone.
ALTER TABLE telemetry_events ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';
UPDATE telemetry_events SET fingerprint = 'event:' || id WHERE kind != 'log';

-- An issue resolved by its owner: its events up to through_id. A later
-- event of the issue opens it again.
CREATE TABLE resolved_issues (
	app_id INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	through_id INTEGER NOT NULL,
	resolved_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	PRIMARY KEY (app_id, fingerprint)
) WITHOUT ROWID;
