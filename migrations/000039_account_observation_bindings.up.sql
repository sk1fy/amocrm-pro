-- Operator-approved catalog bindings for observation only; no lifecycle or entitlement effects.
CREATE TABLE platform_observation_bindings (
    installation_id UUID PRIMARY KEY,
    integration_id UUID NOT NULL,
    amo_account_id BIGINT NOT NULL CHECK (amo_account_id > 0),
    product_code TEXT NOT NULL CHECK (length(product_code) BETWEEN 1 AND 128),
    FOREIGN KEY (installation_id, integration_id) REFERENCES installations(id, integration_id)
);
