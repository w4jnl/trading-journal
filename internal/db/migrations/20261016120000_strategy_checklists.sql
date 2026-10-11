-- migrate:up
-- Playbook checklists: a strategy's rules as tick items. An entry that
-- carries the strategy records which items the trader ticked, so the
-- misses are visible on the entry and compliance can be measured per
-- strategy. Items keep their id across edits of the list (matched by
-- text), so old entries keep their ticks.
CREATE TABLE strategy_checklist_items (
    id          INTEGER PRIMARY KEY,
    strategy_id INTEGER NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    text        TEXT    NOT NULL
);
CREATE INDEX idx_checklist_items_strategy ON strategy_checklist_items(strategy_id, position);

CREATE TABLE journal_entry_checks (
    journal_entry_id INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
    item_id          INTEGER NOT NULL REFERENCES strategy_checklist_items(id) ON DELETE CASCADE,
    PRIMARY KEY (journal_entry_id, item_id)
);

-- migrate:down
DROP TABLE journal_entry_checks;
DROP TABLE strategy_checklist_items;
