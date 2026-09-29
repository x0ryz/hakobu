-- Resource limits for an app and its worker; 0 means no limit.
ALTER TABLE apps ADD COLUMN memory_mb INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN cpus REAL NOT NULL DEFAULT 0;

DROP VIEW app_view;
CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage, a.share_volumes, a.memory_mb, a.cpus
FROM apps a JOIN projects p ON p.id = a.project_id;
