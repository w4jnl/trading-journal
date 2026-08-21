-- migrate:up
-- Per-user sticky view state, one row per (user, page). The value is a
-- canonical relative URL rebuilt server-side from validated fields (never
-- raw client input); bare entry links (/gex, /gex/exposure) 303 to it.
CREATE TABLE user_view_prefs (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    page       TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (user_id, page)
);

-- migrate:down
DROP TABLE user_view_prefs;
