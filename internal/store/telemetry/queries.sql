-- name: CreateTelemetryEvent :exec
INSERT INTO telemetry_events (app_name, kind, level, message, payload, trace_id) VALUES (?, ?, ?, ?, ?, ?);

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

-- name: CountTraceRoutes :one
SELECT count(*) FROM trace_routes WHERE app_name = ? AND hour = ?;

-- name: HasTraceRoute :one
SELECT EXISTS (SELECT 1 FROM trace_routes WHERE app_name = ? AND hour = ? AND name = ?);

-- name: CountTrace :exec
INSERT INTO trace_routes (app_name, name, hour, count, errors, total_ms, b0, b1, b2, b3, b4, b5, b6, b7, b8, b9, b10)
VALUES (@app_name, @name, @hour, 1, @errors, @ms,
	@bucket = 0, @bucket = 1, @bucket = 2, @bucket = 3, @bucket = 4, @bucket = 5,
	@bucket = 6, @bucket = 7, @bucket = 8, @bucket = 9, @bucket = 10)
ON CONFLICT (app_name, hour, name) DO UPDATE SET
	count = count + 1, errors = errors + excluded.errors, total_ms = total_ms + excluded.total_ms,
	b0 = b0 + excluded.b0, b1 = b1 + excluded.b1, b2 = b2 + excluded.b2, b3 = b3 + excluded.b3,
	b4 = b4 + excluded.b4, b5 = b5 + excluded.b5, b6 = b6 + excluded.b6, b7 = b7 + excluded.b7,
	b8 = b8 + excluded.b8, b9 = b9 + excluded.b9, b10 = b10 + excluded.b10;

-- name: ListTraceRoutes :many
SELECT * FROM trace_routes WHERE app_name = ? AND hour >= ?;

-- name: CreateTrace :exec
INSERT INTO traces (app_name, trace_id, name, status, http_status, duration_ms, slow_span, payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListTraces :many
SELECT id, app_name, trace_id, name, status, http_status, duration_ms, slow_span, created_at
FROM traces WHERE app_name = ? ORDER BY id DESC LIMIT ?;

-- name: GetTrace :one
SELECT * FROM traces WHERE id = ? AND app_name = ?;

-- name: TraceByTraceID :one
SELECT id FROM traces WHERE app_name = ? AND trace_id = ? ORDER BY id DESC LIMIT 1;

-- name: DeleteTracesOfApp :exec
DELETE FROM traces WHERE app_name = ?;

-- name: DeleteTraceRoutesOfApp :exec
DELETE FROM trace_routes WHERE app_name = ?;

-- name: PruneTraces :exec
DELETE FROM traces WHERE created_at < ?;

-- name: PruneTraceRoutes :exec
DELETE FROM trace_routes WHERE hour < ?;
