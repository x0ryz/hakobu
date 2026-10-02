-- The owner turned the watchdog off: hakobu deploys it on its own
-- otherwise, whenever emails are on and the token allows.
CREATE TABLE watchdog_off (
	id INTEGER PRIMARY KEY CHECK (id = 1)
);
