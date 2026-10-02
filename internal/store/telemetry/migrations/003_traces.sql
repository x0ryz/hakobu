-- Traces an app's Sentry SDK sent. trace_routes counts every request or
-- task by route and hour, with a histogram of durations in milliseconds
-- (b0 under 10, b1 under 25, then 50, 100, 250, 500, 1000, 2500, 5000,
-- 10000, b10 the rest); traces keeps the slow and failed ones whole.
CREATE TABLE trace_routes (
	app_name TEXT NOT NULL,
	name TEXT NOT NULL,
	hour INTEGER NOT NULL,
	count INTEGER NOT NULL DEFAULT 0,
	errors INTEGER NOT NULL DEFAULT 0,
	total_ms INTEGER NOT NULL DEFAULT 0,
	b0 INTEGER NOT NULL DEFAULT 0, b1 INTEGER NOT NULL DEFAULT 0, b2 INTEGER NOT NULL DEFAULT 0,
	b3 INTEGER NOT NULL DEFAULT 0, b4 INTEGER NOT NULL DEFAULT 0, b5 INTEGER NOT NULL DEFAULT 0,
	b6 INTEGER NOT NULL DEFAULT 0, b7 INTEGER NOT NULL DEFAULT 0, b8 INTEGER NOT NULL DEFAULT 0,
	b9 INTEGER NOT NULL DEFAULT 0, b10 INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (app_name, hour, name)
) WITHOUT ROWID;

CREATE TABLE traces (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	trace_id TEXT NOT NULL,
	name TEXT NOT NULL,
	status TEXT NOT NULL,
	http_status INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	slow_span TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE INDEX traces_app ON traces (app_name, id);
CREATE INDEX traces_trace ON traces (trace_id);

-- The trace an error happened in, to link the two.
ALTER TABLE telemetry_events ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';
