-- name: CreateTelemetryEvent :exec
INSERT INTO telemetry_events (app_name, kind, level, message, payload) VALUES (?, ?, ?, ?, ?);

-- name: ListTelemetryEvents :many
SELECT * FROM telemetry_events WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- name: GetTelemetryEvent :one
SELECT * FROM telemetry_events WHERE id = ? AND app_name = ?;

-- name: DeleteTelemetryOfApp :exec
DELETE FROM telemetry_events WHERE app_name = ?;

-- name: LastTelemetryOfKind :one
SELECT created_at FROM telemetry_events WHERE app_name = ? AND kind = ? ORDER BY id DESC LIMIT 1;

-- name: PruneLogs :exec
DELETE FROM telemetry_events WHERE kind = 'log' AND created_at < ?;

-- name: PruneEvents :exec
DELETE FROM telemetry_events WHERE created_at < ?;
