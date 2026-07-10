CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE FUNCTION set_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TABLE integrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code TEXT NOT NULL,
    client_id TEXT NOT NULL,
    client_secret_ciphertext BYTEA NOT NULL,
    client_secret_key_version INTEGER NOT NULL DEFAULT 1,
    redirect_uri TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    webhook_events JSONB NOT NULL DEFAULT '[]'::jsonb,
    settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT integrations_code_key UNIQUE (code),
    CONSTRAINT integrations_client_id_key UNIQUE (client_id),
    CONSTRAINT integrations_code_not_blank CHECK (btrim(code) <> ''),
    CONSTRAINT integrations_client_id_not_blank CHECK (btrim(client_id) <> ''),
    CONSTRAINT integrations_client_secret_not_empty
        CHECK (octet_length(client_secret_ciphertext) > 0),
    CONSTRAINT integrations_client_secret_key_version_positive
        CHECK (client_secret_key_version > 0),
    CONSTRAINT integrations_redirect_uri_not_blank CHECK (btrim(redirect_uri) <> ''),
    CONSTRAINT integrations_status_check
        CHECK (status IN ('active', 'disabled')),
    CONSTRAINT integrations_webhook_events_is_array
        CHECK (jsonb_typeof(webhook_events) = 'array'),
    CONSTRAINT integrations_settings_is_object
        CHECK (jsonb_typeof(settings) = 'object')
);

CREATE TRIGGER integrations_set_updated_at
BEFORE UPDATE ON integrations
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE installations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id UUID NOT NULL
        REFERENCES integrations(id) ON DELETE RESTRICT,
    account_id BIGINT NOT NULL,
    account_domain TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    installed_by BIGINT,
    settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    webhook_key_hash BYTEA,
    webhook_key_ciphertext BYTEA,
    webhook_key_key_version INTEGER,
    webhook_status TEXT NOT NULL DEFAULT 'pending',
    webhook_settings JSONB NOT NULL DEFAULT '[]'::jsonb,
    webhook_checked_at TIMESTAMPTZ,
    webhook_last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT installations_integration_account_key
        UNIQUE (integration_id, account_id),
    CONSTRAINT installations_id_integration_key
        UNIQUE (id, integration_id),
    CONSTRAINT installations_webhook_key_hash_key UNIQUE (webhook_key_hash),
    CONSTRAINT installations_account_id_positive CHECK (account_id > 0),
    CONSTRAINT installations_account_domain_not_blank
        CHECK (btrim(account_domain) <> ''),
    CONSTRAINT installations_installed_by_positive
        CHECK (installed_by IS NULL OR installed_by > 0),
    CONSTRAINT installations_status_check
        CHECK (status IN (
            'pending',
            'authorizing',
            'active',
            'reauth_required',
            'disabled',
            'uninstalled',
            'error'
        )),
    CONSTRAINT installations_settings_is_object
        CHECK (jsonb_typeof(settings) = 'object'),
    CONSTRAINT installations_webhook_key_hash_length
        CHECK (webhook_key_hash IS NULL OR octet_length(webhook_key_hash) = 32),
    CONSTRAINT installations_webhook_key_ciphertext_not_empty
        CHECK (
            webhook_key_ciphertext IS NULL
            OR octet_length(webhook_key_ciphertext) > 0
        ),
    CONSTRAINT installations_webhook_key_ciphertext_version
        CHECK (
            (webhook_key_ciphertext IS NULL AND webhook_key_key_version IS NULL)
            OR
            (
                webhook_key_ciphertext IS NOT NULL
                AND webhook_key_key_version IS NOT NULL
                AND webhook_key_key_version > 0
            )
        ),
    CONSTRAINT installations_webhook_status_check
        CHECK (webhook_status IN (
            'pending',
            'active',
            'disabled',
            'unregistered',
            'error'
        )),
    CONSTRAINT installations_webhook_settings_is_array
        CHECK (jsonb_typeof(webhook_settings) = 'array')
);

CREATE INDEX installations_status_idx
    ON installations (status, updated_at);

CREATE INDEX installations_webhook_reconcile_idx
    ON installations (webhook_checked_at, created_at)
    WHERE status IN ('authorizing', 'active', 'error')
      AND webhook_status IN ('pending', 'active', 'error');

CREATE TRIGGER installations_set_updated_at
BEFORE UPDATE ON installations
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE oauth_states (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id UUID NOT NULL
        REFERENCES integrations(id) ON DELETE CASCADE,
    installation_id UUID,
    state_hash BYTEA NOT NULL,
    return_url TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT oauth_states_state_hash_key UNIQUE (state_hash),
    CONSTRAINT oauth_states_installation_integration_fkey
        FOREIGN KEY (installation_id, integration_id)
        REFERENCES installations(id, integration_id)
        ON DELETE CASCADE,
    CONSTRAINT oauth_states_state_hash_length
        CHECK (octet_length(state_hash) = 32),
    CONSTRAINT oauth_states_metadata_is_object
        CHECK (jsonb_typeof(metadata) = 'object'),
    CONSTRAINT oauth_states_expiry_after_creation
        CHECK (expires_at > created_at),
    CONSTRAINT oauth_states_consumed_after_creation
        CHECK (consumed_at IS NULL OR consumed_at >= created_at),
    CONSTRAINT oauth_states_consumed_before_expiry
        CHECK (consumed_at IS NULL OR consumed_at <= expires_at)
);

CREATE INDEX oauth_states_unconsumed_expiry_idx
    ON oauth_states (expires_at)
    WHERE consumed_at IS NULL;

CREATE INDEX oauth_states_consumed_cleanup_idx
    ON oauth_states (consumed_at)
    WHERE consumed_at IS NOT NULL;

CREATE TABLE oauth_credentials (
    installation_id UUID PRIMARY KEY
        REFERENCES installations(id) ON DELETE CASCADE,
    access_token_ciphertext BYTEA NOT NULL,
    refresh_token_ciphertext BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    token_version BIGINT NOT NULL DEFAULT 1,
    key_version INTEGER NOT NULL,
    refreshed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT oauth_credentials_access_token_not_empty
        CHECK (octet_length(access_token_ciphertext) > 0),
    CONSTRAINT oauth_credentials_refresh_token_not_empty
        CHECK (octet_length(refresh_token_ciphertext) > 0),
    CONSTRAINT oauth_credentials_token_version_positive
        CHECK (token_version > 0),
    CONSTRAINT oauth_credentials_key_version_positive
        CHECK (key_version > 0),
    CONSTRAINT oauth_credentials_refreshed_after_creation
        CHECK (refreshed_at IS NULL OR refreshed_at >= created_at)
);

CREATE INDEX oauth_credentials_expiry_idx
    ON oauth_credentials (expires_at);

CREATE TRIGGER oauth_credentials_set_updated_at
BEFORE UPDATE ON oauth_credentials
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE webhook_deliveries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    request_id UUID NOT NULL DEFAULT gen_random_uuid(),
    content_type TEXT NOT NULL,
    event_settings JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw_body BYTEA NOT NULL,
    body_sha256 BYTEA NOT NULL,
    parse_status TEXT NOT NULL DEFAULT 'pending',
    parse_error TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    parsed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT webhook_deliveries_id_installation_key
        UNIQUE (id, installation_id),
    CONSTRAINT webhook_deliveries_content_type_not_blank
        CHECK (btrim(content_type) <> ''),
    CONSTRAINT webhook_deliveries_event_settings_is_array
        CHECK (jsonb_typeof(event_settings) = 'array'),
    CONSTRAINT webhook_deliveries_body_sha256_length
        CHECK (octet_length(body_sha256) = 32),
    CONSTRAINT webhook_deliveries_parse_status_check
        CHECK (parse_status IN (
            'pending',
            'processing',
            'parsed',
            'invalid',
            'failed'
        )),
    CONSTRAINT webhook_deliveries_parsed_after_received
        CHECK (parsed_at IS NULL OR parsed_at >= received_at)
);

CREATE INDEX webhook_deliveries_pending_idx
    ON webhook_deliveries (received_at)
    WHERE parse_status = 'pending';

CREATE INDEX webhook_deliveries_request_id_idx
    ON webhook_deliveries (request_id, received_at DESC);

CREATE INDEX webhook_deliveries_installation_received_idx
    ON webhook_deliveries (installation_id, received_at DESC);

CREATE INDEX webhook_deliveries_cleanup_idx
    ON webhook_deliveries (updated_at)
    WHERE parse_status IN ('parsed', 'invalid', 'failed');

CREATE TRIGGER webhook_deliveries_set_updated_at
BEFORE UPDATE ON webhook_deliveries
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE inbox_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    delivery_id UUID NOT NULL,
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE RESTRICT,
    entity_type TEXT NOT NULL,
    event_type TEXT NOT NULL,
    entity_id BIGINT,
    event_at TIMESTAMPTZ,
    payload JSONB NOT NULL,
    deduplication_key BYTEA NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT inbox_events_delivery_installation_fkey
        FOREIGN KEY (delivery_id, installation_id)
        REFERENCES webhook_deliveries(id, installation_id)
        ON DELETE RESTRICT,
    CONSTRAINT inbox_events_installation_deduplication_key
        UNIQUE (installation_id, deduplication_key),
    CONSTRAINT inbox_events_entity_type_not_blank
        CHECK (btrim(entity_type) <> ''),
    CONSTRAINT inbox_events_event_type_not_blank
        CHECK (btrim(event_type) <> ''),
    CONSTRAINT inbox_events_entity_id_positive
        CHECK (entity_id IS NULL OR entity_id > 0),
    CONSTRAINT inbox_events_payload_is_object
        CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT inbox_events_deduplication_key_length
        CHECK (octet_length(deduplication_key) = 32),
    CONSTRAINT inbox_events_status_check
        CHECK (status IN (
            'pending',
            'processing',
            'processed',
            'failed',
            'dead',
            'ignored'
        )),
    CONSTRAINT inbox_events_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT inbox_events_processed_after_creation
        CHECK (processed_at IS NULL OR processed_at >= created_at)
);

CREATE INDEX inbox_events_pending_idx
    ON inbox_events (created_at)
    WHERE status = 'pending';

CREATE INDEX inbox_events_installation_created_idx
    ON inbox_events (installation_id, created_at DESC);

CREATE INDEX inbox_events_cleanup_idx
    ON inbox_events (updated_at)
    WHERE status IN ('processed', 'dead', 'ignored');

CREATE TRIGGER inbox_events_set_updated_at
BEFORE UPDATE ON inbox_events
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID
        REFERENCES installations(id) ON DELETE SET NULL,
    type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    priority SMALLINT NOT NULL DEFAULT 100,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    result JSONB,
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    run_after TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by TEXT,
    locked_until TIMESTAMPTZ,
    last_error_code TEXT,
    last_error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,

    CONSTRAINT jobs_type_not_blank CHECK (btrim(type) <> ''),
    CONSTRAINT jobs_status_check
        CHECK (status IN (
            'queued',
            'processing',
            'retry',
            'completed',
            'failed',
            'dead',
            'cancelled'
        )),
    CONSTRAINT jobs_priority_nonnegative CHECK (priority >= 0),
    CONSTRAINT jobs_payload_is_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT jobs_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT jobs_max_attempts_positive CHECK (max_attempts > 0),
    CONSTRAINT jobs_locked_by_not_blank
        CHECK (locked_by IS NULL OR btrim(locked_by) <> ''),
    CONSTRAINT jobs_lock_pair
        CHECK ((locked_by IS NULL) = (locked_until IS NULL)),
    CONSTRAINT jobs_finished_after_creation
        CHECK (finished_at IS NULL OR finished_at >= created_at)
);

CREATE INDEX jobs_ready_idx
    ON jobs (priority, run_after, created_at)
    WHERE status IN ('queued', 'retry');

CREATE INDEX jobs_processing_lease_idx
    ON jobs (locked_until)
    WHERE status = 'processing' AND locked_until IS NOT NULL;

CREATE INDEX jobs_installation_created_idx
    ON jobs (installation_id, created_at DESC)
    WHERE installation_id IS NOT NULL;

CREATE INDEX jobs_cleanup_idx
    ON jobs (finished_at)
    WHERE status IN ('completed', 'failed', 'dead', 'cancelled')
      AND finished_at IS NOT NULL;

CREATE TRIGGER jobs_set_updated_at
BEFORE UPDATE ON jobs
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE job_attempts (
    id BIGSERIAL PRIMARY KEY,
    job_id UUID NOT NULL
        REFERENCES jobs(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL,
    worker_id TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    outcome TEXT,
    error_code TEXT,
    error_message TEXT,
    duration_ms BIGINT,

    CONSTRAINT job_attempts_job_attempt_key UNIQUE (job_id, attempt),
    CONSTRAINT job_attempts_attempt_positive CHECK (attempt > 0),
    CONSTRAINT job_attempts_worker_id_not_blank CHECK (btrim(worker_id) <> ''),
    CONSTRAINT job_attempts_outcome_check
        CHECK (
            outcome IS NULL
            OR outcome IN (
                'completed',
                'retry',
                'failed',
                'dead',
                'cancelled',
                'lease_expired'
            )
        ),
    CONSTRAINT job_attempts_finished_after_started
        CHECK (finished_at IS NULL OR finished_at >= started_at),
    CONSTRAINT job_attempts_duration_nonnegative
        CHECK (duration_ms IS NULL OR duration_ms >= 0)
);

CREATE INDEX job_attempts_job_started_idx
    ON job_attempts (job_id, started_at DESC);

CREATE TABLE used_widget_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id UUID NOT NULL
        REFERENCES integrations(id) ON DELETE CASCADE,
    jti TEXT NOT NULL,
    issuer TEXT NOT NULL,
    account_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT used_widget_tokens_integration_jti_key
        UNIQUE (integration_id, jti),
    CONSTRAINT used_widget_tokens_jti_not_blank CHECK (btrim(jti) <> ''),
    CONSTRAINT used_widget_tokens_issuer_not_blank CHECK (btrim(issuer) <> ''),
    CONSTRAINT used_widget_tokens_account_id_positive CHECK (account_id > 0),
    CONSTRAINT used_widget_tokens_user_id_positive CHECK (user_id > 0),
    CONSTRAINT used_widget_tokens_expiry_after_creation
        CHECK (expires_at > created_at)
);

CREATE INDEX used_widget_tokens_cleanup_idx
    ON used_widget_tokens (expires_at);

CREATE TABLE idempotency_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL
        REFERENCES installations(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    key_hash BYTEA NOT NULL,
    request_hash BYTEA NOT NULL,
    status TEXT NOT NULL DEFAULT 'processing',
    job_id UUID
        REFERENCES jobs(id) ON DELETE SET NULL,
    response_status INTEGER,
    response_body JSONB,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT idempotency_keys_installation_scope_key_hash_key
        UNIQUE (installation_id, scope, key_hash),
    CONSTRAINT idempotency_keys_scope_not_blank CHECK (btrim(scope) <> ''),
    CONSTRAINT idempotency_keys_key_hash_length
        CHECK (octet_length(key_hash) = 32),
    CONSTRAINT idempotency_keys_request_hash_length
        CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_keys_status_check
        CHECK (status IN ('processing', 'completed', 'failed')),
    CONSTRAINT idempotency_keys_response_status_check
        CHECK (
            response_status IS NULL
            OR response_status BETWEEN 100 AND 599
        ),
    CONSTRAINT idempotency_keys_expiry_after_creation
        CHECK (expires_at > created_at)
);

CREATE INDEX idempotency_keys_processing_idx
    ON idempotency_keys (created_at)
    WHERE status = 'processing';

CREATE INDEX idempotency_keys_cleanup_idx
    ON idempotency_keys (expires_at)
    WHERE status IN ('completed', 'failed');

CREATE TRIGGER idempotency_keys_set_updated_at
BEFORE UPDATE ON idempotency_keys
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE audit_log (
    id BIGSERIAL PRIMARY KEY,
    installation_id UUID
        REFERENCES installations(id) ON DELETE SET NULL,
    actor_type TEXT NOT NULL,
    actor_id TEXT,
    action TEXT NOT NULL,
    object_type TEXT,
    object_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT audit_log_actor_type_not_blank CHECK (btrim(actor_type) <> ''),
    CONSTRAINT audit_log_action_not_blank CHECK (btrim(action) <> ''),
    CONSTRAINT audit_log_metadata_is_object
        CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_log_installation_created_idx
    ON audit_log (installation_id, created_at DESC)
    WHERE installation_id IS NOT NULL;

CREATE INDEX audit_log_action_created_idx
    ON audit_log (action, created_at DESC);
