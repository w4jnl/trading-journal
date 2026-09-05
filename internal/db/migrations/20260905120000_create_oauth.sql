-- migrate:up

-- OAuth 2.1 authorization server for /mcp. AI connectors that cannot hold a
-- static API key (ChatGPT, claude.ai, Claude Code) register themselves as
-- clients (RFC 7591), the signed-in user approves them once at
-- /oauth/authorize, and the resulting opaque tokens are accepted on /mcp.
-- Every secret (authorization code, access token, refresh token) is stored
-- only as sha256 in a UNIQUE column, exactly like api_keys: the values are
-- high-entropy, so a fast hash plus O(1) lookup is correct. Times are
-- written in UTC and compared as text against CURRENT_TIMESTAMP.

-- One row per dynamic registration. Each ChatGPT / claude.ai / Claude Code
-- install gets its own client_id ("tjc_..."). Public clients only: there is
-- no secret column; PKCE protects the code exchange instead.
CREATE TABLE oauth_clients (
    id            INTEGER PRIMARY KEY,
    client_id     TEXT NOT NULL UNIQUE,
    client_name   TEXT,
    redirect_uris TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One row per "user approved client X". This is the revocation unit: every
-- code and token hangs off a grant, so revoking the grant (Settings ->
-- Connected apps, or refresh-token reuse) cuts off that client at once.
CREATE TABLE oauth_grants (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id    TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
    scope        TEXT,
    resource     TEXT,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME,
    revoked_at   DATETIME
);
CREATE INDEX idx_oauth_grants_user ON oauth_grants(user_id);

-- Single-use authorization codes (about a minute of life). The code is bound
-- to the exact redirect_uri presented at /oauth/authorize and to the PKCE
-- challenge, both re-checked at /oauth/token. consumed_at is set by a
-- conditional UPDATE so a replayed code is detected without a read-then-write
-- race. client_id and user_id are denormalized for single-hop checks.
CREATE TABLE oauth_auth_codes (
    id                    INTEGER PRIMARY KEY,
    code_sha256           TEXT NOT NULL UNIQUE,
    grant_id              INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    client_id             TEXT NOT NULL,
    user_id               INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    redirect_uri          TEXT NOT NULL,
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL DEFAULT 'S256',
    resource              TEXT,
    scope                 TEXT,
    expires_at            DATETIME NOT NULL,
    consumed_at           DATETIME,
    created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_oauth_codes_grant ON oauth_auth_codes(grant_id);

-- Bearer tokens presented on /mcp ("tjo_..."). resource records the audience
-- the token was minted for; the /mcp middleware rejects any other value.
CREATE TABLE oauth_access_tokens (
    id           INTEGER PRIMARY KEY,
    token_sha256 TEXT NOT NULL UNIQUE,
    grant_id     INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    client_id    TEXT NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource     TEXT,
    scope        TEXT,
    expires_at   DATETIME NOT NULL,
    revoked_at   DATETIME,
    last_used_at DATETIME,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_oauth_access_tokens_grant ON oauth_access_tokens(grant_id);

-- Rotating refresh tokens ("tjr_..."). Each use revokes the presented token
-- (rotated_to = sha256 of its successor) and issues a new pair; presenting an
-- already-rotated token is treated as theft and revokes the whole grant.
CREATE TABLE oauth_refresh_tokens (
    id           INTEGER PRIMARY KEY,
    token_sha256 TEXT NOT NULL UNIQUE,
    grant_id     INTEGER NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
    client_id    TEXT NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource     TEXT,
    scope        TEXT,
    expires_at   DATETIME NOT NULL,
    revoked_at   DATETIME,
    rotated_to   TEXT,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_oauth_refresh_tokens_grant ON oauth_refresh_tokens(grant_id);

-- migrate:down
DROP INDEX idx_oauth_refresh_tokens_grant;
DROP TABLE oauth_refresh_tokens;
DROP INDEX idx_oauth_access_tokens_grant;
DROP TABLE oauth_access_tokens;
DROP INDEX idx_oauth_codes_grant;
DROP TABLE oauth_auth_codes;
DROP INDEX idx_oauth_grants_user;
DROP TABLE oauth_grants;
DROP TABLE oauth_clients;
