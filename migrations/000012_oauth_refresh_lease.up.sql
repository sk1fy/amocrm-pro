-- Short-lived refresh claim so Core does not hold FOR UPDATE across amoCRM HTTP.
ALTER TABLE oauth_credentials
    ADD COLUMN lease_token UUID,
    ADD COLUMN lease_until TIMESTAMPTZ;

ALTER TABLE oauth_credentials
    ADD CONSTRAINT oauth_credentials_lease_pair
    CHECK ((lease_token IS NULL) = (lease_until IS NULL));
