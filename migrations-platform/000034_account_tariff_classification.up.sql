CREATE TABLE platform_manager.account_tariff_classification_changes (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts(id),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    previous TEXT CHECK (previous IN ('Свободный клиент','Клиент партнёра','Наш клиент','Запрет работы')),
    previous_partner_name TEXT CHECK (char_length(btrim(previous_partner_name)) BETWEEN 1 AND 200),
    current TEXT CHECK (current IN ('Свободный клиент','Клиент партнёра','Наш клиент','Запрет работы')),
    current_partner_name TEXT CHECK (char_length(btrim(current_partner_name)) BETWEEN 1 AND 200),
    reason TEXT NOT NULL,
    tariff_id BIGINT NOT NULL CHECK (tariff_id BETWEEN 1 AND 9007199254740991),
    installation_id UUID NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    authorization_sequence BIGINT NOT NULL CHECK (authorization_sequence > 0),
    occurred_at TIMESTAMPTZ NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (account_id,version),
    CHECK ((previous IS NOT DISTINCT FROM 'Клиент партнёра') = (previous_partner_name IS NOT NULL)),
    CHECK ((current IS NOT DISTINCT FROM 'Клиент партнёра') = (current_partner_name IS NOT NULL)),
    CHECK (CASE reason
        WHEN 'technical_tariff_detected' THEN tariff_id = 22000000
            AND previous IS DISTINCT FROM 'Запрет работы' AND current IS NOT DISTINCT FROM 'Запрет работы'
        WHEN 'ordinary_tariff_detected' THEN tariff_id <> 22000000
            AND previous IS NOT DISTINCT FROM 'Запрет работы' AND current IS DISTINCT FROM 'Запрет работы'
        ELSE FALSE END)
);

ALTER TABLE platform_manager.account_profiles
    ADD COLUMN technical_classification_version BIGINT,
    ADD FOREIGN KEY (account_id,technical_classification_version)
        REFERENCES platform_manager.account_tariff_classification_changes(account_id,version),
    ADD CHECK (technical_classification_version IS NULL OR classification IS NOT DISTINCT FROM 'Запрет работы');

CREATE FUNCTION platform_manager.clear_inactive_technical_classification() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    IF NEW.classification IS DISTINCT FROM 'Запрет работы' THEN
        NEW.technical_classification_version := NULL;
    END IF;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.clear_inactive_technical_classification() FROM PUBLIC;

-- Older Platform binaries can change the profile without writing the marker.
CREATE TRIGGER clear_inactive_technical_classification
BEFORE UPDATE OF classification ON platform_manager.account_profiles
FOR EACH ROW EXECUTE FUNCTION platform_manager.clear_inactive_technical_classification();

CREATE OR REPLACE VIEW platform_manager.account_classification_changes AS
SELECT account_id, version, occurred_at,
       CASE kind WHEN 'account_profile_updated' THEN 'manual' ELSE 'subscription' END AS source,
       details #>> '{before,classification}' AS previous,
       details #>> '{after,classification}' AS current,
       NULL::text AS reason
FROM platform_manager.account_change_events
WHERE kind IN ('account_profile_updated','subscription_granted','subscription_renewed')
  AND details->'before' ? 'classification' AND details->'after' ? 'classification'
  AND (details #>> '{before,classification}' IS DISTINCT FROM details #>> '{after,classification}'
       OR (kind = 'account_profile_updated' AND details->'classification_confirmed' = 'true'::jsonb))
UNION ALL
SELECT account_id, version, occurred_at, 'widget_tariff', previous, current, reason
FROM platform_manager.account_tariff_classification_changes;

GRANT SELECT, INSERT ON platform_manager.account_tariff_classification_changes TO platform_jobs;
GRANT UPDATE (technical_classification_version) ON platform_manager.account_profiles TO platform_jobs;
