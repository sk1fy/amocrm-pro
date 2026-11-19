ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind='installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(account_id,product_id,resource_id)=0)
        OR (kind='account.refresh' AND account_id IS NOT NULL AND num_nonnulls(episode_id,expected_version,product_id,resource_id)=0)
        OR (kind IN ('account.profile','account.portrait','account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,product_id,resource_id)=0)
        OR (kind='account.renewal' AND num_nonnulls(account_id,product_id,expected_version)=3 AND num_nonnulls(episode_id,resource_id)=0)
        OR (kind IN ('portrait.template','installation.deal_settings.set') AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,resource_id)=0)
        OR (kind='portrait.niche.create' AND num_nonnulls(episode_id,account_id,product_id,resource_id,expected_version)=0)
        OR (kind IN ('portrait.niche.update','portrait.niche.delete','portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id)=0)
        OR (kind='product.create' AND resource_id IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,expected_version)=0)
        OR (kind IN ('product.update','product.company_workflow.set') AND product_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id,account_id,resource_id)=0));

CREATE TABLE platform_company.installation_deal_settings_revisions (
 scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 revision BIGINT NOT NULL CHECK (revision BETWEEN 1 AND 9007199254740990),
 metadata_generation UUID NOT NULL,
 component TEXT NOT NULL DEFAULT 'metadata' CHECK (component='metadata'),
 pipeline_id BIGINT NOT NULL,
 stage_id BIGINT NOT NULL,
 command_id UUID NOT NULL UNIQUE REFERENCES platform_manager.commands,
 actor_id UUID NOT NULL,
 validated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 PRIMARY KEY (scope_id,revision),
 FOREIGN KEY (metadata_generation,scope_id,component) REFERENCES platform_company.generations(id,scope_id,component),
 FOREIGN KEY (metadata_generation,pipeline_id,stage_id) REFERENCES platform_company.workflow_stages
);

GRANT SELECT, INSERT ON platform_company.installation_deal_settings_revisions TO platform_jobs;
