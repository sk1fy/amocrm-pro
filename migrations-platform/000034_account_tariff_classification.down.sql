UPDATE platform_manager.account_profiles AS p SET
    classification=c.previous, partner_name=c.previous_partner_name,
    technical_classification_version=NULL, version=p.version+1
FROM platform_manager.account_tariff_classification_changes AS c
WHERE (c.account_id,c.version)=(p.account_id,p.technical_classification_version);

DROP VIEW platform_manager.account_classification_changes;
DROP TRIGGER clear_inactive_technical_classification ON platform_manager.account_profiles;
DROP FUNCTION platform_manager.clear_inactive_technical_classification();
ALTER TABLE platform_manager.account_profiles DROP COLUMN technical_classification_version;
DROP TABLE platform_manager.account_tariff_classification_changes;

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
