-- What the watchdog was last deployed with: the targets it checks (JSON)
-- and the D1 database it records their history in ('' without one).
ALTER TABLE watchdog ADD COLUMN targets TEXT NOT NULL DEFAULT '';
ALTER TABLE watchdog ADD COLUMN d1_database_id TEXT NOT NULL DEFAULT '';
