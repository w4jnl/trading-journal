-- migrate:up
-- "Personal" and the legacy "live" both describe the same thing: a real-money
-- account that isn't a prop-firm challenge. Fold "personal" into "live"
-- (which becomes the canonical slug going forward).
UPDATE accounts SET type = 'live' WHERE type = 'personal';

-- migrate:down
-- No-op: we can't tell which "live" rows used to be "personal".
