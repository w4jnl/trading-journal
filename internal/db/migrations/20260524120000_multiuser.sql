-- migrate:up

-- Users + sessions. user_id is added to the ownership-root tables
-- (accounts, journal_templates, sync_log); everything else reaches
-- the user via FK chain through accounts.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT,
    -- NULL when auth.mode = "proxy" and the user was provisioned via header
    password_hash TEXT,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    is_active     INTEGER NOT NULL DEFAULT 1,
    display_name  TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_login_at DATETIME
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL,
    user_agent TEXT,
    ip         TEXT
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- Create the bootstrap user. The runner replaces ${BOOTSTRAP_USERNAME}
-- with cfg.Auth.BootstrapUser before exec; see internal/db/migrations.go.
INSERT INTO users (username, is_admin, display_name)
VALUES ('${BOOTSTRAP_USERNAME}', 1, '${BOOTSTRAP_USERNAME}');

-- accounts: add user_id, change unique key to (user_id, import_source,
-- account_ref) so two users can each own an account with the same broker ref.
CREATE TABLE accounts_new (
    id            INTEGER PRIMARY KEY,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    firm          TEXT,
    import_source TEXT NOT NULL,
    account_ref   TEXT NOT NULL,
    type          TEXT,
    currency      TEXT NOT NULL DEFAULT 'USD',
    needs_setup   INTEGER NOT NULL DEFAULT 0,
    active        INTEGER NOT NULL DEFAULT 1,
    commission_source TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, import_source, account_ref)
);
INSERT INTO accounts_new (id, user_id, name, firm, import_source, account_ref, type, currency, needs_setup, active, commission_source, created_at)
SELECT a.id,
       (SELECT id FROM users WHERE username = '${BOOTSTRAP_USERNAME}'),
       a.name, a.firm, a.import_source, a.account_ref, a.type, a.currency,
       a.needs_setup, a.active, a.commission_source, a.created_at
FROM accounts a;
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
CREATE INDEX idx_accounts_user ON accounts(user_id);

-- journal_templates: builtins stay NULL (global); user-created take user_id.
-- Drop the global UNIQUE(name) constraint (per-user templates may share names
-- with other users' templates or with builtins).
CREATE TABLE journal_templates_new (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    scope      TEXT NOT NULL,
    body       TEXT NOT NULL,
    builtin    INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO journal_templates_new (id, user_id, name, scope, body, builtin, created_at)
SELECT id,
       CASE WHEN builtin = 1 THEN NULL
            ELSE (SELECT id FROM users WHERE username = '${BOOTSTRAP_USERNAME}') END,
       name, scope, body, builtin, created_at
FROM journal_templates;
DROP TABLE journal_templates;
ALTER TABLE journal_templates_new RENAME TO journal_templates;
CREATE INDEX idx_jtemplates_user_scope ON journal_templates(user_id, scope);

-- sync_log: tag each import with the user that owned the inbox. Existing
-- rows belong to the bootstrap user (they predate multi-user).
ALTER TABLE sync_log ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
UPDATE sync_log SET user_id = (SELECT id FROM users WHERE username = '${BOOTSTRAP_USERNAME}') WHERE user_id IS NULL;
CREATE INDEX idx_synclog_user ON sync_log(user_id);

-- migrate:down
DROP INDEX idx_synclog_user;
ALTER TABLE sync_log DROP COLUMN user_id;
DROP INDEX idx_jtemplates_user_scope;
DROP TABLE journal_templates;
CREATE TABLE journal_templates (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    scope   TEXT NOT NULL,
    body    TEXT NOT NULL,
    builtin INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
DROP INDEX idx_accounts_user;
DROP TABLE accounts;
CREATE TABLE accounts (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    firm          TEXT,
    import_source TEXT NOT NULL,
    account_ref   TEXT NOT NULL,
    type          TEXT,
    currency      TEXT NOT NULL DEFAULT 'USD',
    needs_setup   INTEGER NOT NULL DEFAULT 0,
    active        INTEGER NOT NULL DEFAULT 1,
    commission_source TEXT,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (import_source, account_ref)
);
DROP INDEX idx_sessions_expires;
DROP INDEX idx_sessions_user;
DROP TABLE sessions;
DROP TABLE users;
