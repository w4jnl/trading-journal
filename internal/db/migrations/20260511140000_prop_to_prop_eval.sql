-- migrate:up
-- Fold the legacy `prop` slug into prop_eval. Anyone who flagged an account as
-- "prop" before the eval/funded/live split is most likely on the eval rung
-- (that's where new prop traders start). Funded/live users will re-set as needed.
UPDATE accounts SET type = 'prop_eval' WHERE type = 'prop';

-- migrate:down
-- No-op: original eval/funded/live distinction is gone after this collapse.
