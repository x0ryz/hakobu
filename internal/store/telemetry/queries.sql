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

-- name: PutSample :exec
INSERT INTO samples (target, res, slot, ts, cpu, cpu_max, cpu_limit, mem, mem_max, mem_limit, net_rx, net_tx, disk_read, disk_write, load, disk_used, disk_total)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (target, res, slot) DO UPDATE SET
	ts = excluded.ts, cpu = excluded.cpu, cpu_max = excluded.cpu_max, cpu_limit = excluded.cpu_limit,
	mem = excluded.mem, mem_max = excluded.mem_max, mem_limit = excluded.mem_limit,
	net_rx = excluded.net_rx, net_tx = excluded.net_tx, disk_read = excluded.disk_read, disk_write = excluded.disk_write,
	load = excluded.load, disk_used = excluded.disk_used, disk_total = excluded.disk_total;

-- name: SummarizeSamples :many
SELECT target,
	CAST(avg(cpu) AS REAL) AS cpu, CAST(max(cpu_max) AS REAL) AS cpu_max, CAST(max(cpu_limit) AS REAL) AS cpu_limit,
	CAST(avg(mem) AS INTEGER) AS mem, CAST(max(mem_max) AS INTEGER) AS mem_max, CAST(max(mem_limit) AS INTEGER) AS mem_limit,
	CAST(avg(net_rx) AS REAL) AS net_rx, CAST(avg(net_tx) AS REAL) AS net_tx,
	CAST(avg(disk_read) AS REAL) AS disk_read, CAST(avg(disk_write) AS REAL) AS disk_write,
	CAST(avg(load) AS REAL) AS load, CAST(max(disk_used) AS INTEGER) AS disk_used, CAST(max(disk_total) AS INTEGER) AS disk_total
FROM samples WHERE res = ? AND ts >= ? AND ts < ? GROUP BY target;

-- name: ListSamples :many
SELECT * FROM samples WHERE target = ? AND res = ? AND ts >= ? ORDER BY ts;

-- name: LatestSamples :many
SELECT * FROM samples WHERE res = ? AND ts >= ? ORDER BY ts;

-- name: DeleteSamplesOf :exec
DELETE FROM samples WHERE target = ?;

-- name: PruneSamples :exec
DELETE FROM samples WHERE ts < ?;
