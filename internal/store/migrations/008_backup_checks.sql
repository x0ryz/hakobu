-- The storage a backup was uploaded to (the database's backup storage may
-- change later; '' for backups from before this column, which went to the
-- current one), and the result of restoring it into a scratch database:
-- verified_at '' means not checked yet, verify_error '' means it restored.
ALTER TABLE backups ADD COLUMN storage TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN verified_at TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN verify_error TEXT NOT NULL DEFAULT '';
ALTER TABLE backups ADD COLUMN tables INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_backups_database ON backups(database);
