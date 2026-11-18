DROP SCHEMA platform_company CASCADE;
ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind='installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(account_id,product_id,resource_id)=0)
        OR (kind='account.refresh' AND account_id IS NOT NULL AND num_nonnulls(episode_id,expected_version,product_id,resource_id)=0)
        OR (kind IN ('account.profile','account.portrait','account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,product_id,resource_id)=0)
        OR (kind='account.renewal' AND num_nonnulls(account_id,product_id,expected_version)=3 AND num_nonnulls(episode_id,resource_id)=0)
        OR (kind='portrait.template' AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,resource_id)=0)
        OR (kind='portrait.niche.create' AND num_nonnulls(episode_id,account_id,product_id,resource_id,expected_version)=0)
        OR (kind IN ('portrait.niche.update','portrait.niche.delete','portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id)=0)
        OR (kind='product.create' AND resource_id IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,expected_version)=0)
        OR (kind='product.update' AND product_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id,account_id,resource_id)=0));
