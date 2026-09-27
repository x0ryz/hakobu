-- Persistent Docker volumes mounted into an app and its worker. An app with
-- volumes deploys by stopping the old version first, unless share_volumes
-- lets both versions use them during a zero-downtime switch.
CREATE TABLE volumes (
	app_name TEXT NOT NULL,
	name TEXT NOT NULL,
	mount_path TEXT NOT NULL,
	PRIMARY KEY (app_name, name)
);

ALTER TABLE apps ADD COLUMN share_volumes INTEGER NOT NULL DEFAULT 0;

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage, a.share_volumes
FROM apps a JOIN projects p ON p.id = a.project_id;
