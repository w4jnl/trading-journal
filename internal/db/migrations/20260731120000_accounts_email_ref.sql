-- migrate:up
-- Alternate account identifier used ONLY by the MW email ingest. The AMP/CQG
-- connection is the reason this exists: MotiveWave's fill-notification email
-- reports the CQG-internal account id (e.g. 1234567) while the CSV export's
-- Account column carries the AMP account number (e.g. 120042) - verified by
-- joining the same Order ID across both sources. Rithmic and IBKR
-- connections use the same ref in email and CSV, so this stays NULL for
-- them. Set via the accounts UI; matched by the email ingest alongside
-- account_ref (an exact account_ref match wins over an alias match).
ALTER TABLE accounts ADD COLUMN email_ref TEXT;

-- migrate:down
ALTER TABLE accounts DROP COLUMN email_ref;
