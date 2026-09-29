CREATE TABLE projects (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT UNIQUE NOT NULL,
	shared_env TEXT NOT NULL DEFAULT ''
);

-- port is the app's proxy on 127.0.0.1; container_port 0 means "detect",
-- and live_port is the port the running container was found listening on.
-- dns_* is the record hakobu owns for domain. memory_mb/cpus 0: no limit.
-- An app with volumes deploys by stopping the old version first, unless
-- share_volumes lets both versions use them during the switch.
-- snapshot_db/snapshot_at describe data/snapshots/<app>.sql.gz, the dump of
-- the app's database taken before the deploy of its live image, so Rollback
-- can return the data together with the :previous image.
CREATE TABLE apps (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	name TEXT UNIQUE NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	domain TEXT NOT NULL DEFAULT '',
	dns_zone_id TEXT NOT NULL DEFAULT '',
	dns_record_id TEXT NOT NULL DEFAULT '',
	port INTEGER NOT NULL,
	container_port INTEGER NOT NULL DEFAULT 0,
	live_port INTEGER NOT NULL DEFAULT 0,
	build_path TEXT NOT NULL DEFAULT '',
	build_strategy TEXT NOT NULL DEFAULT 'railpack',
	active_slot TEXT NOT NULL DEFAULT 'blue',
	env TEXT NOT NULL DEFAULT '',
	sentry_key TEXT NOT NULL DEFAULT '',
	health_check_path TEXT NOT NULL DEFAULT '/',
	linked_db TEXT NOT NULL DEFAULT '',
	linked_storage TEXT NOT NULL DEFAULT '',
	share_volumes INTEGER NOT NULL DEFAULT 0,
	memory_mb INTEGER NOT NULL DEFAULT 0,
	cpus REAL NOT NULL DEFAULT 0,
	snapshot_db TEXT NOT NULL DEFAULT '',
	snapshot_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_apps_repo ON apps(repo);

CREATE VIEW app_view AS
SELECT a.id, a.project_id, p.name AS project_name, a.name, a.repo, a.domain, a.dns_zone_id, a.dns_record_id,
	a.port, a.container_port, a.live_port, a.build_path, a.build_strategy, a.active_slot, a.env, a.sentry_key,
	a.health_check_path, a.linked_db, a.linked_storage, a.share_volumes, a.memory_mb, a.cpus,
	a.snapshot_db, a.snapshot_at
FROM apps a JOIN projects p ON p.id = a.project_id;

CREATE TABLE workers (
	app_name TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	command TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT ''
);

-- Docker volumes mounted into an app and its worker.
CREATE TABLE volumes (
	app_name TEXT NOT NULL,
	name TEXT NOT NULL,
	mount_path TEXT NOT NULL,
	PRIMARY KEY (app_name, name)
);

CREATE TABLE databases (
	name TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	db_user TEXT NOT NULL,
	db_password TEXT NOT NULL
);

CREATE TABLE storages (
	name TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id),
	provider TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	endpoint TEXT NOT NULL DEFAULT '',
	access_key_id TEXT NOT NULL,
	secret_access_key TEXT NOT NULL,
	bucket TEXT NOT NULL,
	region TEXT NOT NULL DEFAULT 'auto'
);

-- A backup is the files <object_key>/000, /001, ... in the R2 backup bucket
-- (one upload is capped at 300 MB). The check restores it into a scratch
-- database: verified_at '' means not checked yet, verify_error '' means it
-- restored.
CREATE TABLE backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	database TEXT NOT NULL,
	object_key TEXT NOT NULL,
	parts INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	verified_at TEXT NOT NULL DEFAULT '',
	verify_error TEXT NOT NULL DEFAULT '',
	tables INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_backups_database ON backups(database);

CREATE TABLE github_app (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	app_id INTEGER NOT NULL,
	slug TEXT NOT NULL,
	private_key TEXT NOT NULL,
	webhook_secret TEXT NOT NULL,
	client_id TEXT NOT NULL,
	client_secret TEXT NOT NULL
);

-- The GitHub login that claimed the panel; only it can sign in.
CREATE TABLE owner (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	github_login TEXT NOT NULL
);

-- id is the SHA-256 of the session cookie.
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	github_login TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

CREATE TABLE deploy_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	trigger_source TEXT NOT NULL,
	status TEXT NOT NULL,
	output TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE telemetry_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	kind TEXT NOT NULL,
	level TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- The Cloudflare account connected through OAuth, the tunnel hakobu created
-- in it, the DNS record of the panel and the R2 bucket for database
-- backups ('' until backups are set up).
CREATE TABLE cloudflare (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	access_token TEXT NOT NULL,
	refresh_token TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	tunnel_id TEXT NOT NULL DEFAULT '',
	tunnel_token TEXT NOT NULL DEFAULT '',
	panel_zone_id TEXT NOT NULL DEFAULT '',
	panel_record_id TEXT NOT NULL DEFAULT '',
	backup_bucket TEXT NOT NULL DEFAULT ''
);
