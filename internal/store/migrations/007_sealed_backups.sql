-- Backups are sealed with a key of their own (secret.NewFileWriterKey),
-- kept here encrypted with the master key: a rotation of the master key
-- reaches it, while the bucket's lock keeps hakobu from rewriting the
-- backup itself. '' for dumps made before, stored as they are.
ALTER TABLE backups ADD COLUMN file_key TEXT NOT NULL DEFAULT '';

-- Backups of apps' volumes: a tar of the volume, sealed like the dumps.
CREATE TABLE volume_backups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	app_name TEXT NOT NULL,
	volume TEXT NOT NULL,
	object_key TEXT NOT NULL,
	parts INTEGER NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	-- of the sealed file as uploaded, checked before it's restored
	sha256 TEXT NOT NULL,
	file_key TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	verified_at TEXT NOT NULL DEFAULT '',
	verify_error TEXT NOT NULL DEFAULT '',
	files INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_volume_backups_volume ON volume_backups(app_name, volume);
