-- Projects

-- name: CreateProject :exec
INSERT INTO projects (name) VALUES (?);

-- name: GetProject :one
SELECT * FROM projects WHERE name = ?;

-- name: GetProjectByID :one
SELECT * FROM projects WHERE id = ?;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY name;

-- name: SetProjectSharedEnv :exec
UPDATE projects SET shared_env = ? WHERE name = ?;

-- name: DeleteProject :exec
DELETE FROM projects WHERE name = ?;

-- Apps

-- name: CreateApp :exec
-- The port is the next free one for the app's proxy, starting at 8081.
INSERT INTO apps (project_id, name, repo, port, container_port, build_path, build_strategy)
VALUES (?, ?, ?, (SELECT MAX(COALESCE(MAX(port), 8080), 8080) + 1 FROM apps), ?, ?, ?);

-- name: GetApp :one
SELECT * FROM app_view WHERE name = ?;

-- name: GetAppByID :one
SELECT * FROM app_view WHERE id = ?;

-- name: GetAppByDomain :one
SELECT * FROM app_view WHERE domain = ? AND domain != '';

-- name: ListApps :many
SELECT * FROM app_view ORDER BY name;

-- name: ListAppsByProject :many
SELECT * FROM app_view WHERE project_id = ? ORDER BY name;

-- name: ListAppsByRepo :many
SELECT * FROM app_view WHERE repo = ?;

-- name: SetAppSettings :exec
UPDATE apps SET container_port = ?, health_check_path = ? WHERE name = ?;

-- name: SetAppDomain :exec
UPDATE apps SET domain = ?, dns_zone_id = ?, dns_record_id = ? WHERE name = ?;

-- name: SetAppLive :exec
UPDATE apps SET active_slot = ?, live_port = ? WHERE name = ?;

-- name: SetAppEnv :exec
UPDATE apps SET env = ? WHERE name = ?;

-- name: SetAppSentryKey :exec
UPDATE apps SET sentry_key = ? WHERE name = ?;

-- name: SetAppLinkedDB :exec
UPDATE apps SET linked_db = ? WHERE name = ?;

-- name: SetAppLinkedStorage :exec
UPDATE apps SET linked_storage = ? WHERE name = ?;

-- name: AppsUsingDatabase :many
SELECT name FROM apps WHERE linked_db = ? ORDER BY name;

-- name: AppsUsingStorage :many
SELECT name FROM apps WHERE linked_storage = ? ORDER BY name;

-- name: DeleteApp :exec
DELETE FROM apps WHERE name = ?;

-- name: SetAppShareVolumes :exec
UPDATE apps SET share_volumes = ? WHERE name = ?;

-- name: SetAppLimits :exec
UPDATE apps SET memory_mb = ?, cpus = ? WHERE name = ?;

-- Volumes

-- name: AddVolume :exec
INSERT INTO volumes (app_name, name, mount_path) VALUES (?, ?, ?);

-- name: ListVolumes :many
SELECT * FROM volumes WHERE app_name = ? ORDER BY name;

-- name: ListAllVolumes :many
SELECT * FROM volumes;

-- name: DeleteVolume :exec
DELETE FROM volumes WHERE app_name = ? AND name = ?;

-- name: DeleteVolumesOfApp :exec
DELETE FROM volumes WHERE app_name = ?;

-- Workers

-- name: SaveWorker :exec
INSERT INTO workers (app_name, name, command, env) VALUES (?, ?, ?, ?)
ON CONFLICT(app_name) DO UPDATE SET name = excluded.name, command = excluded.command, env = excluded.env;

-- name: GetWorker :one
SELECT * FROM workers WHERE app_name = ?;

-- name: DeleteWorker :exec
DELETE FROM workers WHERE app_name = ?;

-- Databases

-- name: CreateDatabase :exec
INSERT INTO databases (name, project_id, db_user, db_password) VALUES (?, ?, ?, ?);

-- name: GetDatabase :one
SELECT * FROM databases WHERE name = ?;

-- name: ListDatabases :many
SELECT * FROM databases ORDER BY name;

-- name: ListDatabasesByProject :many
SELECT * FROM databases WHERE project_id = ? ORDER BY name;

-- name: DeleteDatabase :exec
DELETE FROM databases WHERE name = ?;

-- Storages

-- name: CreateStorage :exec
INSERT INTO storages (name, project_id, provider, account_id, endpoint, access_key_id, secret_access_key, bucket, region)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetStorage :one
SELECT * FROM storages WHERE name = ?;

-- name: ListStoragesByProject :many
SELECT * FROM storages WHERE project_id = ? ORDER BY name;

-- name: DeleteStorage :exec
DELETE FROM storages WHERE name = ?;

-- Backups

-- name: CreateBackup :one
INSERT INTO backups (database, object_key, parts, size_bytes) VALUES (?, ?, ?, ?) RETURNING id;

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ListBackups :many
SELECT * FROM backups WHERE database = ? ORDER BY id DESC LIMIT ?;

-- name: ListAllBackups :many
SELECT * FROM backups WHERE database = ? ORDER BY id DESC;

-- name: SetBackupVerified :exec
UPDATE backups SET verified_at = ?, verify_error = ?, tables = ? WHERE id = ?;

-- name: DeleteBackup :exec
DELETE FROM backups WHERE id = ?;

-- name: DeleteBackupsOf :exec
DELETE FROM backups WHERE database = ?;

-- GitHub App and the owner

-- name: SaveGitHubApp :exec
INSERT OR REPLACE INTO github_app (id, app_id, slug, private_key, webhook_secret, client_id, client_secret)
VALUES (1, ?, ?, ?, ?, ?, ?);

-- name: GetGitHubApp :one
SELECT * FROM github_app WHERE id = 1;

-- name: GetOwner :one
SELECT github_login FROM owner WHERE id = 1;

-- name: SetOwner :exec
INSERT INTO owner (id, github_login) VALUES (1, ?);

-- name: CreateSession :exec
INSERT INTO sessions (id, github_login, expires_at) VALUES (?, ?, ?);

-- name: GetSessionRow :one
SELECT * FROM sessions WHERE id = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- Deploy logs

-- name: CreateDeployLog :one
INSERT INTO deploy_logs (app_name, trigger_source, status) VALUES (?, ?, ?) RETURNING id;

-- name: UpdateDeployLog :exec
UPDATE deploy_logs SET status = ?, output = ? WHERE id = ?;

-- name: ListDeployLogs :many
SELECT * FROM deploy_logs WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- name: FailRunningDeployLogs :exec
UPDATE deploy_logs SET status = 'failed', output = output || char(10) || 'interrupted: agent restarted' WHERE status = 'running';

-- name: DeleteDeployLogsOfApp :exec
DELETE FROM deploy_logs WHERE app_name = ?;

-- name: PruneDeployLogs :exec
DELETE FROM deploy_logs WHERE created_at < ?;

-- Telemetry

-- name: CreateTelemetryEvent :exec
INSERT INTO telemetry_events (app_name, kind, level, message, payload) VALUES (?, ?, ?, ?, ?);

-- name: ListTelemetryEvents :many
SELECT * FROM telemetry_events WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- name: DeleteTelemetryOfApp :exec
DELETE FROM telemetry_events WHERE app_name = ?;

-- name: LastTelemetryOfKind :one
SELECT created_at FROM telemetry_events WHERE app_name = ? AND kind = ? ORDER BY id DESC LIMIT 1;

-- name: PruneTelemetry :exec
DELETE FROM telemetry_events WHERE created_at < ?;

-- Cloudflare

-- name: GetCloudflare :one
SELECT * FROM cloudflare WHERE id = 1;

-- name: SaveCloudflareToken :exec
INSERT INTO cloudflare (id, access_token, refresh_token, expires_at) VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET access_token = excluded.access_token, refresh_token = excluded.refresh_token, expires_at = excluded.expires_at;

-- name: SetBackupBucket :exec
UPDATE cloudflare SET backup_bucket = ? WHERE id = 1;

-- name: SaveCloudflareTunnel :exec
UPDATE cloudflare SET account_id = ?, tunnel_id = ?, tunnel_token = ?, panel_zone_id = ?, panel_record_id = ? WHERE id = 1;
