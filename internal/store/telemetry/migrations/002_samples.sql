-- Resource usage of the server (target 'host'), the apps ('app:<name>'),
-- their workers ('worker:<name>') and hakobu's services ('service:<name>'),
-- as rings of fixed slots: res 60 keeps 48 hours of minutes, res 300 a
-- month of five-minute averages and peaks. Writing a slot replaces what
-- was there, so the table never grows; readers skip slots by ts.
-- CPU is in cores, memory in bytes, the rates in bytes per second; a limit
-- of 0 means none.
CREATE TABLE samples (
	target TEXT NOT NULL,
	res INTEGER NOT NULL,
	slot INTEGER NOT NULL,
	ts INTEGER NOT NULL,
	cpu REAL NOT NULL DEFAULT 0,
	cpu_max REAL NOT NULL DEFAULT 0,
	cpu_limit REAL NOT NULL DEFAULT 0,
	mem INTEGER NOT NULL DEFAULT 0,
	mem_max INTEGER NOT NULL DEFAULT 0,
	mem_limit INTEGER NOT NULL DEFAULT 0,
	net_rx REAL NOT NULL DEFAULT 0,
	net_tx REAL NOT NULL DEFAULT 0,
	disk_read REAL NOT NULL DEFAULT 0,
	disk_write REAL NOT NULL DEFAULT 0,
	load REAL NOT NULL DEFAULT 0,
	disk_used INTEGER NOT NULL DEFAULT 0,
	disk_total INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (target, res, slot)
) WITHOUT ROWID;
