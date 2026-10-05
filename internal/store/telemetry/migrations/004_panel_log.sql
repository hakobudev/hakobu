-- The panel's own log (internal/panellog), for Settings → Panel log.
-- level: 0 info, 1 warn, 2 error. Kept as long as the apps' events.
CREATE TABLE panel_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	level INTEGER NOT NULL,
	message TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX panel_log_created ON panel_log (created_at);
