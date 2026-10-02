-- Email to the owner when something goes wrong: where it goes and the
-- domain it's sent from (one with Cloudflare Email Routing on).
CREATE TABLE notify (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	email TEXT NOT NULL,
	sender_domain TEXT NOT NULL
);
