-- Store ownership before external registration so retries and base-URL/key
-- rotation never need to assume the entire account's webhook list is ours.
CREATE TABLE installation_webhook_destinations (
    installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
    destination_hash bytea NOT NULL CHECK (octet_length(destination_hash)=32),
    destination_ciphertext bytea NOT NULL,
    key_version integer NOT NULL CHECK (key_version>0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id,destination_hash)
);
CREATE INDEX lead_status_rule_configurations_cleanup_idx
    ON lead_status_workflow_rule_configurations(configured_at,job_id);
