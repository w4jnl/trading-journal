-- migrate:up
-- Extend instruments with the (asset_class, size_class) categorization that
-- drives commission lookup. Nullable: stub rows created by the importer for
-- unknown symbols stay uncategorized until the user assigns them via /commissions.
ALTER TABLE instruments ADD COLUMN asset_class TEXT REFERENCES asset_classes(code);
ALTER TABLE instruments ADD COLUMN size_class  TEXT REFERENCES size_classes(code);

-- Backfill the symbols seeded in 20260510130000. Anything else (auto-stubbed
-- by importer) stays NULL and surfaces in the diagnostics banner.
UPDATE instruments SET asset_class='equity_futures', size_class='mini'     WHERE symbol_root IN ('ES','NQ');
UPDATE instruments SET asset_class='equity_futures', size_class='micro'    WHERE symbol_root IN ('MES','MNQ');
UPDATE instruments SET asset_class='energy_futures', size_class='standard' WHERE symbol_root='CL';
UPDATE instruments SET asset_class='energy_futures', size_class='micro'    WHERE symbol_root='MCL';

-- migrate:down
-- SQLite < 3.35 can't DROP COLUMN. Down is best-effort: clear the values.
UPDATE instruments SET asset_class=NULL, size_class=NULL;
