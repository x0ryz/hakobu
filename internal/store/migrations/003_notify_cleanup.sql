-- The owner's primary email on GitHub, offered for notifications.
ALTER TABLE owner ADD COLUMN github_email TEXT NOT NULL DEFAULT '';

-- What hakobu set up in Cloudflare for the emails, to undo it when they're
-- turned off: the address it added to the account and the domain it
-- turned Email Routing on for ('' if they were there already).
ALTER TABLE notify ADD COLUMN zone_id TEXT NOT NULL DEFAULT '';
ALTER TABLE notify ADD COLUMN added_address INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notify ADD COLUMN routed_domain TEXT NOT NULL DEFAULT '';
