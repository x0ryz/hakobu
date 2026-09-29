-- Sessions are now stored by the hash of their token; the old rows can't
-- be matched any more, so everyone signs in again once.
DELETE FROM sessions;
