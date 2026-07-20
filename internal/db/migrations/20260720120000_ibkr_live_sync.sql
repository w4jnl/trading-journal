-- migrate:up
-- IBKR live sync via Client Portal Gateway(s). Gateways are per-user data
-- managed in the settings UI (one user can run several, one per IB login);
-- they hold no secrets - the gateway session itself is established by the
-- user's browser login outside the app. Runtime status (connected /
-- needs-login / unreachable) is kept in memory by the sync service.
CREATE TABLE ibkr_gateways (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    url          TEXT NOT NULL,
    tls_insecure INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (user_id, name)
);

-- live_sync_since is the cutover instant that partitions an account's fills
-- between CSV imports and live API sync: MotiveWave rows at/after it are
-- skipped on import (the API owns that era), API fills before it are skipped
-- on sync (the CSV already delivered them under different exec ids - the two
-- id namespaces cannot be deduplicated against each other). NULL = account
-- not live-synced, CSV imports untouched.
ALTER TABLE accounts ADD COLUMN live_sync_since DATETIME;

-- migrate:down
ALTER TABLE accounts DROP COLUMN live_sync_since;
DROP TABLE ibkr_gateways;
