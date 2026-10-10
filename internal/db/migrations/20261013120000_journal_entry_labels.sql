-- migrate:up
-- Structured review labels on journal entries (P5): what went wrong
-- (kind = mistake) and how it felt (kind = emotion), from a fixed
-- vocabulary kept in code (internal/journal/labels.go) so analytics can
-- group by them. One row per (entry, kind, label); an entry carries any
-- number of each kind.
CREATE TABLE journal_entry_labels (
    journal_entry_id INTEGER NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    label            TEXT NOT NULL,
    PRIMARY KEY (journal_entry_id, kind, label)
);
CREATE INDEX idx_jel_kind_label ON journal_entry_labels(kind, label);

-- migrate:down
DROP INDEX IF EXISTS idx_jel_kind_label;
DROP TABLE IF EXISTS journal_entry_labels;
