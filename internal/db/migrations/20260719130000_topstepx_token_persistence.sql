-- migrate:up
-- Persist the TopstepX session token so an app restart reuses it instead of
-- re-running loginKey, and so a background maintainer can renew it via
-- /api/Auth/validate before it expires. The token is a bearer credential, so
-- it is stored AES-256-GCM encrypted with the same key as the apiKey.
-- token_expires_at is the JWT exp claim (UTC). All three are NULL until the
-- first login (and reset to NULL when credentials are replaced or a token is
-- rejected). This is a separate migration because 20260719120000 is already
-- applied to a live DB.
ALTER TABLE topstepx_credentials ADD COLUMN token_enc BLOB;
ALTER TABLE topstepx_credentials ADD COLUMN token_nonce BLOB;
ALTER TABLE topstepx_credentials ADD COLUMN token_expires_at DATETIME;

-- migrate:down
ALTER TABLE topstepx_credentials DROP COLUMN token_expires_at;
ALTER TABLE topstepx_credentials DROP COLUMN token_nonce;
ALTER TABLE topstepx_credentials DROP COLUMN token_enc;
