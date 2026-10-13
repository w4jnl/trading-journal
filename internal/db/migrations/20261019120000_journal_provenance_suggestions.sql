-- migrate:up
-- Who set what, and what the in-app AI only suggests.
--
-- tag_source / strategy_source on journal_entries and source on
-- journal_entry_labels record who set a value: 'trader' (a page editor, or a
-- suggestion the trader accepted) or 'api' (written through the REST API or
-- MCP, typically by an AI tool, never confirmed in an editor). NULL on the
-- entry columns means legacy or unset; existing labels count as the trader's.
-- Analytics can leave the 'api' values out ("confirmed only").
--
-- journal_suggestions holds what an in-app AI review proposes for a note
-- (tag, strategy, mistake and emotion labels) instead of writing it: the
-- trader accepts one (it becomes the field, source 'trader', and the row
-- goes) or dismisses it (status 'dismissed', kept so a later review does not
-- propose it again). value is the tag word, the strategy id, or the label key.
ALTER TABLE journal_entries ADD COLUMN tag_source TEXT;
ALTER TABLE journal_entries ADD COLUMN strategy_source TEXT;
ALTER TABLE journal_entry_labels ADD COLUMN source TEXT NOT NULL DEFAULT 'trader';

CREATE TABLE journal_suggestions (
    journal_entry_id INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    value            TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'open',
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (journal_entry_id, kind, value)
);

-- migrate:down
DROP TABLE journal_suggestions;
ALTER TABLE journal_entry_labels DROP COLUMN source;
ALTER TABLE journal_entries DROP COLUMN strategy_source;
ALTER TABLE journal_entries DROP COLUMN tag_source;
