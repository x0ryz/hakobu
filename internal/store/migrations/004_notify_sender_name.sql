-- The part of the sender's address before the @, chosen in Settings.
ALTER TABLE notify ADD COLUMN sender_name TEXT NOT NULL DEFAULT 'alerts';
