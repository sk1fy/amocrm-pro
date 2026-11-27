CREATE TABLE platform_manager.account_tariff_fields (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id) ON DELETE CASCADE,
    field TEXT NOT NULL,
    numeric_value BIGINT,
    text_value TEXT,
    installation_id UUID NOT NULL,
    generation BIGINT NOT NULL,
    authorization_sequence BIGINT NOT NULL CHECK (authorization_sequence > 0),
    observed_at TIMESTAMPTZ NOT NULL,
    trial_reset_at TIMESTAMPTZ,
    source TEXT NOT NULL DEFAULT 'widget' CHECK (source = 'widget'),
    PRIMARY KEY (account_id, field),
    FOREIGN KEY (installation_id, generation) REFERENCES platform_lifecycle.episodes(installation_id, generation) ON DELETE CASCADE,
    CHECK (CASE field
        WHEN 'tariff_id' THEN numeric_value IS NOT NULL AND numeric_value BETWEEN 1 AND 9007199254740991
            AND text_value IS NULL AND trial_reset_at IS NULL
        WHEN 'trial_end' THEN numeric_value IS NOT NULL AND numeric_value BETWEEN 1 AND 253402300799
            AND text_value IS NULL AND trial_reset_at IS NULL
        WHEN 'pay_type' THEN numeric_value IS NULL AND text_value IS NOT NULL AND octet_length(text_value) BETWEEN 1 AND 64
            AND (trial_reset_at IS NULL OR trial_reset_at <= observed_at)
        ELSE FALSE END)
);
GRANT SELECT, INSERT ON platform_manager.account_tariff_fields TO platform_jobs;
GRANT UPDATE (numeric_value, text_value, installation_id, generation, authorization_sequence, observed_at, trial_reset_at)
    ON platform_manager.account_tariff_fields TO platform_jobs;

GRANT UPDATE (max_attempts) ON platform_jobs.jobs TO platform_jobs;
