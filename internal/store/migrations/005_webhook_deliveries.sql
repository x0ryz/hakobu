-- GitHub webhook deliveries already handled, so a captured one sent again
-- doesn't deploy again. Kept a few days; older pushes are refused anyway.
CREATE TABLE webhook_deliveries (
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
