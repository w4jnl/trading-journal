-- migrate:up

-- Journal attachments — images (chart screenshots) referenced from inside a
-- journal entry's markdown body via ![](/journal/attachments/<id>). The
-- record stores metadata; the bytes live on disk under cfg.Attachments.Root.
-- entry_id is intentionally NOT a FK: an upload that happens before the
-- entry is created (paste-on-blank-page) still gets stored, and a 0 sentinel
-- lets the handler reconcile after the entry's first save. entry_scope is
-- 'day' | 'week' | 'trade' | 'group' to match journal_entries.scope.
CREATE TABLE journal_attachments (
    id          INTEGER PRIMARY KEY,
    entry_id    INTEGER NOT NULL DEFAULT 0,
    entry_scope TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL UNIQUE,
    mime        TEXT NOT NULL,
    byte_size   INTEGER NOT NULL,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_journal_attachments_entry ON journal_attachments(entry_id);

-- Journal templates — canned starting bodies the user can apply from the
-- editor's Templates dropdown. builtin=1 templates are seeded by migration
-- and can't be deleted via the UI; the user can clone-and-edit them.
CREATE TABLE journal_templates (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    scope   TEXT NOT NULL,  -- 'day' | 'week' | 'trade' | 'group' | 'any'
    body    TEXT NOT NULL,
    builtin INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO journal_templates (name, scope, builtin, body) VALUES
('Daily review', 'day', 1, '## What worked
-

## What didn''t
-

## Mistakes to avoid tomorrow
-

## Notes
'),
('Weekly review', 'week', 1, '## Week summary

## Themes / patterns
-

## Wins
-

## Mistakes & recurring issues
-

## Plan for next week
-
'),
('Trade post-mortem', 'trade', 1, '## Setup / thesis

## Entry

## Management

## Exit

## What I''d do differently
'),
('Mistake log', 'any', 1, '**Mistake:**

**Why it happened:**

**Cost:**

**Rule going forward:**
');

-- migrate:down
DROP INDEX IF EXISTS idx_journal_attachments_entry;
DROP TABLE IF EXISTS journal_templates;
DROP TABLE IF EXISTS journal_attachments;
