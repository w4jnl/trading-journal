-- migrate:up
-- Retained AI reads of a gamma snapshot (Market > Gamma page). One row per
-- (user, market, snapshot) so an analysis is generated once and then kept:
-- the engine's at-or-before semantics mean many requested moments resolve to
-- the same snapshot, and re-reading one costs an LLM call for text that
-- cannot change - the snapshot is immutable once captured.
--
-- as_of is TEXT, not DATETIME, and holds the snapshot's own as_of formatted
-- RFC3339Nano in UTC. This column is an equality KEY, so a canonical string
-- the app controls is safer than relying on driver datetime formatting to
-- round-trip microseconds identically.
CREATE TABLE gex_analyses (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    market     TEXT NOT NULL,
    as_of      TEXT NOT NULL,
    model      TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_gex_analyses_snapshot ON gex_analyses(user_id, market, as_of);

-- migrate:down
DROP INDEX idx_gex_analyses_snapshot;
DROP TABLE gex_analyses;
