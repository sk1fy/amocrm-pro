ALTER TABLE platform_manager.account_refresh_slots
    DROP CONSTRAINT account_refresh_slots_component_check,
    ADD CHECK (component IN ('users','license','installations','amostart'));
ALTER TABLE platform_manager.account_refresh_work
    DROP CONSTRAINT account_refresh_work_component_check,
    ADD CHECK (component IN ('users','license','amostart'));

CREATE TABLE platform_manager.account_amostart_licenses (
    account_id uuid PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    license_until timestamptz NOT NULL,
    tariff text NOT NULL CHECK (char_length(tariff) BETWEEN 1 AND 512),
    observed_at timestamptz NOT NULL,
    evidence_id uuid NOT NULL
);
GRANT SELECT, INSERT, UPDATE ON platform_manager.account_amostart_licenses TO platform_jobs;

CREATE VIEW platform_manager.account_classification_changes AS
SELECT account_id, version, occurred_at,
       CASE kind WHEN 'account_profile_updated' THEN 'manual' ELSE 'subscription' END AS source,
       details #>> '{before,classification}' AS previous,
       details #>> '{after,classification}' AS current
FROM platform_manager.account_change_events
WHERE kind IN ('account_profile_updated','subscription_granted','subscription_renewed')
  AND details->'before' ? 'classification' AND details->'after' ? 'classification'
  AND details #>> '{before,classification}' IS DISTINCT FROM details #>> '{after,classification}';
GRANT SELECT ON platform_manager.account_classification_changes TO platform_jobs;
