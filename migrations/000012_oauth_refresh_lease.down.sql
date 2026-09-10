-- Drops the refresh lease columns. Down does not restore rotated tokens.
ALTER TABLE oauth_credentials
    DROP CONSTRAINT IF EXISTS oauth_credentials_lease_pair;

ALTER TABLE oauth_credentials
    DROP COLUMN IF EXISTS lease_token,
    DROP COLUMN IF EXISTS lease_until;
