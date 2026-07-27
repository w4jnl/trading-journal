-- migrate:up
-- Per-row import provenance + broker order id, for the MW email ingest phase.
--
-- order_id: the broker order number when the source carries one. MotiveWave
-- "Executions" CSVs have an "Order ID" column and MW order-fill emails carry
-- the same value, so (account_id, order_id) is the deterministic join key that
-- lets a CSV import supersede provisional email-ingested rows without any
-- fuzzy matching. NULL when the source has no order id (old-format CSVs,
-- TopstepX, manual entries).
--
-- import_method: which pipeline wrote the row. Values:
--   motivewave_csv | topstepx_csv | topstepx_trades_csv | topstepx_api |
--   ibkr_api | mw_email | manual
-- Rows with import_method = 'mw_email' are PROVISIONAL: near-real-time
-- snapshots from MotiveWave fill notifications, replaced when the
-- authoritative CSV import arrives.
ALTER TABLE executions ADD COLUMN order_id TEXT;
ALTER TABLE executions ADD COLUMN import_method TEXT NOT NULL DEFAULT '';

-- Backfill provenance for existing rows, most specific rule first.
UPDATE executions SET import_method = 'manual'
WHERE manual = 1 OR broker_exec_id LIKE 'MANUAL-%';

-- TopstepX trades-CSV writes synthetic execution pairs suffixed -entry/-exit.
UPDATE executions SET import_method = 'topstepx_trades_csv'
WHERE import_method = ''
  AND (broker_exec_id LIKE '%-entry' OR broker_exec_id LIKE '%-exit');

-- IBKR gateway sync marshals {"conid":...} (json.Marshal sorts keys).
UPDATE executions SET import_method = 'ibkr_api'
WHERE import_method = '' AND raw_data LIKE '{"conid":%';

-- TopstepX API sync stores a JSON array of constituent half-trades.
UPDATE executions SET import_method = 'topstepx_api'
WHERE import_method = '' AND raw_data LIKE '[%'
  AND account_id IN (SELECT id FROM accounts WHERE import_source = 'topstepx_csv');

UPDATE executions SET import_method = 'topstepx_csv'
WHERE import_method = ''
  AND account_id IN (SELECT id FROM accounts WHERE import_source = 'topstepx_csv');

UPDATE executions SET import_method = 'motivewave_csv'
WHERE import_method = '';

-- Recover order ids already preserved verbatim in raw_data by past MW CSV
-- imports (the parser ignored the column but stored the full row as JSON).
UPDATE executions
SET order_id = json_extract(raw_data, '$."Order ID"')
WHERE import_method = 'motivewave_csv'
  AND raw_data LIKE '{%'
  AND json_extract(raw_data, '$."Order ID"') IS NOT NULL
  AND json_extract(raw_data, '$."Order ID"') != '';

CREATE INDEX idx_executions_account_order
ON executions(account_id, order_id)
WHERE order_id IS NOT NULL;

-- migrate:down
DROP INDEX idx_executions_account_order;
ALTER TABLE executions DROP COLUMN import_method;
ALTER TABLE executions DROP COLUMN order_id;
