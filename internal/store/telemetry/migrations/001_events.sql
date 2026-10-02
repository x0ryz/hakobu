-- What apps sent through their Sentry SDK (errors, logs) and what hakobu
-- saw happen to them (crashes, out-of-memory kills, health check outages).
-- Moved here from the panel's database, which is backed up; this one isn't.
CREATE TABLE telemetry_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	kind TEXT NOT NULL,
	level TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX telemetry_events_app ON telemetry_events (app_name, id);
CREATE INDEX telemetry_events_created ON telemetry_events (created_at);
