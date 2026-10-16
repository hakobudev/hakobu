-- The databases' ('db:<id>') and caches' ('cache:<id>') checks, one row
-- each time the panel asked their server (ops.checkData): up 1 when it
-- answered, else why not. A time the server wasn't connected has no row.
-- Kept as long as the apps' events.
CREATE TABLE data_checks (
	target TEXT NOT NULL,
	ts INTEGER NOT NULL,
	up INTEGER NOT NULL,
	why TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (target, ts)
) WITHOUT ROWID;
