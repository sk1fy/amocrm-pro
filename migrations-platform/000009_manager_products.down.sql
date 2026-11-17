LOCK TABLE platform_lifecycle.products,platform_manager.commands,platform_lifecycle.verified_tariff_versions,platform_lifecycle.verified_trial_policies IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
    IF EXISTS(SELECT FROM platform_lifecycle.products WHERE catalog_version IS NOT NULL)
       OR EXISTS(SELECT FROM platform_manager.commands WHERE kind IN ('product.create','product.update'))
       OR EXISTS(SELECT FROM platform_lifecycle.verified_tariff_versions)
       OR EXISTS(SELECT FROM platform_lifecycle.verified_trial_policies v WHERE NOT EXISTS(
           SELECT FROM platform_lifecycle.initial_term_policies live
           WHERE (live.product_id,live.version,live.rule,live.evidence)=(v.product_id,v.version,v.rule,v.evidence)))
    THEN RAISE EXCEPTION 'catalog is populated; retain P08 or export and explicitly reconcile catalog state before downgrade';
    END IF;
END $$;
DROP FUNCTION platform_manager.apply_catalog_policy(UUID,BIGINT);
DROP FUNCTION platform_manager.adopt_trial_evidence(UUID);
DROP TRIGGER require_history_catalog_policy ON platform_lifecycle.product_trial_revisions;
DROP TRIGGER catalog_trial_history_revision ON platform_lifecycle.product_trial_revisions;
DROP TRIGGER catalog_policy_revision ON platform_lifecycle.initial_term_policies;
DROP TRIGGER catalog_integrations_revision ON platform_lifecycle.integrations;
DROP TRIGGER catalog_products_revision ON platform_lifecycle.products;
DROP FUNCTION platform_manager.advance_product_catalog();
DROP TABLE platform_manager.product_catalog_revision;
DROP TRIGGER require_live_catalog_policy ON platform_lifecycle.initial_term_policies;
DROP TRIGGER require_product_catalog_policy ON platform_lifecycle.products;
DROP FUNCTION platform_lifecycle.require_catalog_policy();
ALTER TABLE platform_lifecycle.products DROP CONSTRAINT product_catalog_complete,
    DROP CONSTRAINT product_trial_revision, DROP CONSTRAINT product_tariff_evidence,
    DROP COLUMN catalog_policy_version,DROP COLUMN tariff_version,DROP COLUMN links,DROP COLUMN availability,DROP COLUMN catalog_version;
DROP TABLE platform_lifecycle.product_trial_revisions;
DROP TABLE platform_lifecycle.verified_trial_policies;
DROP TABLE platform_lifecycle.verified_tariff_versions;
REVOKE INSERT, UPDATE, DELETE ON platform_lifecycle.initial_term_policies FROM platform_jobs;
REVOKE INSERT(id,code) ON platform_lifecycle.products FROM platform_jobs;
DROP VIEW platform_manager.product_access;
ALTER TABLE platform_manager.commands DROP CONSTRAINT commands_target_shape;
ALTER TABLE platform_manager.commands ADD CONSTRAINT commands_target_shape CHECK (
        (kind='installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(account_id,product_id,resource_id)=0)
        OR (kind='account.refresh' AND account_id IS NOT NULL AND num_nonnulls(episode_id,expected_version,product_id,resource_id)=0)
        OR (kind IN ('account.profile','account.portrait','account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,product_id,resource_id)=0)
        OR (kind='account.renewal' AND num_nonnulls(account_id,product_id,expected_version)=3 AND num_nonnulls(episode_id,resource_id)=0)
        OR (kind='portrait.template' AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,resource_id)=0)
        OR (kind='portrait.niche.create' AND num_nonnulls(episode_id,account_id,product_id,resource_id,expected_version)=0)
        OR (kind IN ('portrait.niche.update','portrait.niche.delete','portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id)=0));
