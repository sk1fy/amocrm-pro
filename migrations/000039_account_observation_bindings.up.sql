-- Operator-approved catalog bindings for observation only; no lifecycle or entitlement effects.
CREATE TABLE platform_observation_bindings (
    installation_id UUID PRIMARY KEY,
    integration_id UUID NOT NULL,
    amo_account_id BIGINT NOT NULL CHECK (amo_account_id > 0),
    product_code TEXT NOT NULL CHECK (length(product_code) BETWEEN 1 AND 128),
    FOREIGN KEY (installation_id, integration_id) REFERENCES installations(id, integration_id)
);

-- Production grants CRUD on newly created Core tables to the worker by default.
-- Binding approval belongs to the operator; the gateway only needs SELECT.
DO $binding_permissions$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'core_runtime') THEN
        REVOKE INSERT, UPDATE, DELETE ON platform_observation_bindings FROM core_runtime;
        GRANT SELECT ON platform_observation_bindings TO core_runtime;
    END IF;
END
$binding_permissions$;
