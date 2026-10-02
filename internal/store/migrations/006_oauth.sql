-- hakobu is an OAuth authorization server for its MCP endpoint, so Claude
-- and other AI clients can reach the panel on the owner's say-so.
--
-- Clients that registered themselves (Dynamic Client Registration). One
-- identified by a metadata document URL instead isn't stored: the document
-- is fetched whenever it asks for access.
CREATE TABLE oauth_clients (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	redirect_uris TEXT NOT NULL, -- one per line
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- A grant is the owner's approval of one client; its codes and tokens go
-- with it when it's revoked. github_id is the owner who approved it, so a
-- new owner doesn't inherit it.
CREATE TABLE oauth_grants (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id TEXT NOT NULL,
	client_name TEXT NOT NULL,
	redirect_uri TEXT NOT NULL,
	scope TEXT NOT NULL,
	github_id INTEGER NOT NULL,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
	last_used_at TEXT NOT NULL DEFAULT ''
);

-- Authorization codes, access and refresh tokens, stored by the SHA-256 of
-- the value handed out. A code or refresh token is kept, used_at set, after
-- it's exchanged: presenting it again means someone else has a copy.
CREATE TABLE oauth_tokens (
	id TEXT PRIMARY KEY,
	grant_id INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
	kind TEXT NOT NULL CHECK (kind IN ('code', 'access', 'refresh')),
	code_challenge TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL,
	used_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX oauth_tokens_grant ON oauth_tokens(grant_id);
