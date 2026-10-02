-- No automatic grant: this capability is opt-in through existing provisioning.
CREATE TABLE distribution_service_grants (
 key_id TEXT NOT NULL CHECK (key_id ~ '^[a-zA-Z0-9_-]{1,64}$'),
 company_id UUID NOT NULL,
 installation_id UUID NOT NULL REFERENCES installations(id),
 enabled BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY(key_id, company_id, installation_id)
);
CREATE TABLE distribution_service_nonces (
 key_id TEXT NOT NULL, nonce UUID NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(key_id, nonce)
);
CREATE INDEX distribution_nonce_expiry ON distribution_service_nonces(expires_at);
CREATE UNIQUE INDEX installation_distribution_scope ON installations(id,integration_id,account_id);
CREATE TABLE distribution_bindings (
 id UUID PRIMARY KEY, company_id UUID NOT NULL,
 installation_id UUID NOT NULL,
 integration_id UUID NOT NULL REFERENCES integrations(id), account_id BIGINT NOT NULL CHECK(account_id>0),
 revision BIGINT NOT NULL CHECK(revision BETWEEN 1 AND 9007199254740991), intent_id UUID NOT NULL UNIQUE,
 state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','revoked')),
 revoked_at TIMESTAMPTZ,
 mapping_revision BIGINT NOT NULL DEFAULT 0 CHECK(mapping_revision BETWEEN 0 AND 9007199254740991),
 mapping_hash BYTEA,
 FOREIGN KEY(installation_id,integration_id,account_id) REFERENCES installations(id,integration_id,account_id),
 confirmed_by BIGINT NOT NULL CHECK(confirmed_by>0), created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX distribution_active_company ON distribution_bindings(company_id) WHERE state='active';
CREATE UNIQUE INDEX distribution_active_installation ON distribution_bindings(installation_id) WHERE state='active';
CREATE TABLE distribution_actor_mappings (
 binding_id UUID NOT NULL REFERENCES distribution_bindings(id),
 employee_id UUID NOT NULL, user_id BIGINT NOT NULL CHECK(user_id>0),
 PRIMARY KEY(binding_id,employee_id), UNIQUE(binding_id,user_id)
);

CREATE TABLE distribution_binding_tombstones (
 id UUID PRIMARY KEY, company_id UUID NOT NULL,
 installation_id UUID NOT NULL REFERENCES installations(id),
 revoked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
