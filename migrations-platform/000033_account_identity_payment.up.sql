ALTER TABLE platform_manager.account_refresh_slots
    ADD COLUMN payment_required_at TIMESTAMPTZ,
    ADD CONSTRAINT account_refresh_payment_identity_check
        CHECK (payment_required_at IS NULL OR component = 'license');

GRANT UPDATE (payment_required_at) ON platform_manager.account_refresh_slots TO platform_jobs;

CREATE INDEX account_refresh_amostart_attempt_idx ON platform_manager.account_refresh_slots(attempted_at)
    WHERE component = 'amostart';

ALTER TABLE platform_manager.account_tariff_fields
    ADD COLUMN value_changed_at TIMESTAMPTZ,
    ADD COLUMN trial_or_blocked_at TIMESTAMPTZ,
    ADD CONSTRAINT account_tariff_trial_or_blocked_check
        CHECK (trial_or_blocked_at IS NULL OR (field = 'pay_type' AND trial_or_blocked_at <= observed_at));

UPDATE platform_manager.account_tariff_fields SET value_changed_at=observed_at,
    trial_or_blocked_at=CASE WHEN field='pay_type' AND lower(text_value) IN ('trial','blocked') THEN observed_at END;
ALTER TABLE platform_manager.account_tariff_fields
    ALTER COLUMN value_changed_at SET NOT NULL,
    ADD CONSTRAINT account_tariff_value_changed_check CHECK (value_changed_at <= observed_at);
GRANT UPDATE (value_changed_at, trial_or_blocked_at) ON platform_manager.account_tariff_fields TO platform_jobs;
