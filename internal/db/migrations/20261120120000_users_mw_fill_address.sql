-- migrate:up
-- The user's own MotiveWave fill address: the "To" they put in MotiveWave's
-- mail settings so a fill mail names its owner (the journal is the SMTP
-- endpoint, nothing is delivered, the address is a routing key). NULL = none
-- generated; the mail then falls back to the user's e-mail or the only user.
ALTER TABLE users ADD COLUMN mw_fill_address TEXT;
CREATE UNIQUE INDEX users_mw_fill_address ON users(mw_fill_address) WHERE mw_fill_address IS NOT NULL;

-- migrate:down
DROP INDEX users_mw_fill_address;
ALTER TABLE users DROP COLUMN mw_fill_address;
