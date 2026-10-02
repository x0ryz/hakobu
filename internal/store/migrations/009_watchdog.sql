-- The Worker hakobu deployed to the owner's Cloudflare account to watch
-- the panel from outside (ops/watchdog.js), and its KV namespace.
CREATE TABLE watchdog (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	script TEXT NOT NULL,
	kv_namespace_id TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
