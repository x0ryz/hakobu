-- Apps' errors and logs moved to their own database (data/telemetry.db),
-- which isn't backed up; store.OpenWithKey copied them over first.
DROP TABLE telemetry_events;
