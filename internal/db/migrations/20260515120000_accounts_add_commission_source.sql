-- migrate:up
-- commission_source flags how an account's trades acquire their commission:
--   'rate'   - commission_rates lookup (firm + asset_class + size_class) at
--              position-builder time. The default for every account.
--   'inline' - commission is the value the import carried per trade. The
--              position builder skips FIFO-rebuild for these accounts so the
--              CSV-provided commission isn't overwritten.
-- TopstepX trades CSV is the only inline source today; its Fees + Commissions
-- columns are authoritative. TopstepX orders CSV (no commission column) stays
-- on 'rate'.
ALTER TABLE accounts
    ADD COLUMN commission_source TEXT NOT NULL DEFAULT 'rate'
    CHECK (commission_source IN ('rate', 'inline'));

-- Backfill: any account that already has trades with a broker_trade_id was
-- imported via the trades CSV path (CreateImportedTrade is the only writer of
-- that column). Mark those inline so a future rebuild preserves their CSV
-- commissions.
UPDATE accounts
SET commission_source = 'inline'
WHERE id IN (
    SELECT DISTINCT account_id FROM trades WHERE broker_trade_id IS NOT NULL
);

-- migrate:down
-- SQLite < 3.35 can't DROP COLUMN cleanly without table rebuild; leave the
-- column in place on rollback. Existing rows keep their value.
