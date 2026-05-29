-- migrate:up

-- One markdown "playbook" per user: a free-form document describing which
-- strategy to use and when. Singleton per user (user_id is the PK); created
-- lazily on first save via upsert.
CREATE TABLE strategy_playbooks (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    body       TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- migrate:down
DROP TABLE strategy_playbooks;
