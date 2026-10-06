-- Per-group Digital Pipeline credentials. Only sha256(key) and a sealed copy
-- are stored; the plaintext is returned once (idempotent re-issue decrypts it)
-- and is never equal to the shared installation webhook key.
CREATE TABLE distribution_dp_credentials (
    id               uuid PRIMARY KEY,
    installation_id  uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    account_id       bigint NOT NULL,
    group_id         uuid NOT NULL,
    binding_id       uuid NOT NULL,
    binding_revision bigint NOT NULL,
    key_hash         bytea NOT NULL,
    key_ciphertext   bytea NOT NULL,
    key_key_version  integer NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT distribution_dp_credentials_scope_key UNIQUE (installation_id, account_id, group_id),
    CONSTRAINT distribution_dp_credentials_hash_key UNIQUE (key_hash)
);

CREATE INDEX distribution_dp_credentials_installation_idx
    ON distribution_dp_credentials (installation_id, account_id);
