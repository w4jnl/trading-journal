-- migrate:up
-- A plan on a position that is still open. Trades exist only once a cycle
-- closes (positions.Builder writes closed cycles), so the plan of an open
-- position needs a home of its own: one row per position cycle (from flat
-- to flat) on one account and contract, anchored on the fill that OPENED the
-- cycle. Execution ids are stable across FIFO rebuilds (trade ids are not),
-- but the MotiveWave e-mail path replaces its provisional fill with the CSV
-- row later, under a new id; the order id and the time of that fill are the
-- fallback anchors (positions.FindPositionPlan re-points open_exec_id).
--
-- stop is the price the trader intends to exit at a loss, risk the money it
-- implies at the position's size (recomputed when the size changes), target
-- the profit price, thesis a short pre-trade note. source says who set it:
-- 'trader', 'api' (REST/MCP) or 'ai' (an accepted suggestion).
--
-- When the cycle closes the builder copies stop/risk/target onto the new
-- trade (handed_off_at) and links it (trade_id, re-pointed on every
-- rebuild). After the hand-off the trade's own plan is the one edited.
CREATE TABLE position_plans (
    id INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    symbol TEXT NOT NULL,
    open_exec_id INTEGER NOT NULL,
    open_order_id TEXT,
    opened_at DATETIME NOT NULL,
    stop REAL,
    risk REAL,
    target REAL,
    thesis TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'trader',
    trade_id INTEGER REFERENCES trades(id) ON DELETE SET NULL,
    handed_off_at DATETIME,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (account_id, open_exec_id)
);
CREATE INDEX idx_position_plans_order ON position_plans(account_id, open_order_id);
CREATE INDEX idx_position_plans_opened ON position_plans(account_id, symbol, opened_at);

-- migrate:down
DROP TABLE position_plans;
