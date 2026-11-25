ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind='installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(account_id,product_id,resource_id)=0)
        OR (kind='account.refresh' AND account_id IS NOT NULL AND num_nonnulls(episode_id,expected_version,product_id,resource_id)=0)
        OR (kind IN ('account.subscription','account.profile','account.portrait','account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,product_id,resource_id)=0)
        OR (kind='account.renewal' AND num_nonnulls(account_id,product_id,expected_version)=3 AND num_nonnulls(episode_id,resource_id)=0)
        OR (kind IN ('portrait.template','installation.deal_settings.set') AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,resource_id)=0)
        OR (kind='portrait.niche.create' AND num_nonnulls(episode_id,account_id,product_id,resource_id,expected_version)=0)
        OR (kind IN ('portrait.niche.update','portrait.niche.delete','portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id)=0)
        OR (kind='product.create' AND resource_id IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,expected_version)=0)
        OR (kind IN ('product.update','product.company_workflow.set') AND product_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id,account_id,resource_id)=0));

CREATE TABLE platform_manager.account_subscription_revisions (
    account_id UUID NOT NULL REFERENCES platform_lifecycle.accounts,
    version BIGINT NOT NULL,
    plan TEXT NOT NULL CHECK (plan IN ('top','unified')),
    ends_on DATE NOT NULL,
    seats INTEGER NOT NULL CHECK (seats BETWEEN 1 AND 1000000),
    deal_id TEXT NOT NULL CHECK (deal_id ~ '^[1-9][0-9]{0,14}$'),
    action TEXT NOT NULL CHECK (action IN ('grant','renew','seats','revoke')),
    command_id UUID NOT NULL UNIQUE REFERENCES platform_manager.commands,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (account_id,version)
);
GRANT SELECT,INSERT ON platform_manager.account_subscription_revisions TO platform_jobs;

CREATE VIEW platform_manager.account_subscriptions AS
SELECT s.*, (s.ends_on+1)::timestamp AT TIME ZONE 'Europe/Moscow' AS valid_until,
    CASE WHEN action='revoke' THEN 'revoked'
         WHEN ends_on < (statement_timestamp() AT TIME ZONE 'Europe/Moscow')::date THEN 'expired'
         WHEN r.active_count IS NULL THEN 'awaiting_users'
         WHEN r.active_count > seats THEN 'suspended' ELSE 'active' END AS state
FROM (SELECT DISTINCT ON (account_id) account_id,version,plan,ends_on,seats,deal_id,action,changed_at
      FROM platform_manager.account_subscription_revisions ORDER BY account_id,version DESC) s
LEFT JOIN platform_manager.account_rosters r USING(account_id);
GRANT SELECT ON platform_manager.account_subscriptions TO platform_jobs;

CREATE VIEW platform_access.subscription_terms AS
SELECT s.account_id,p.id AS product_id,s.valid_until AS package_until
FROM platform_manager.account_subscriptions s
JOIN platform_lifecycle.products p ON p.line='TOP' OR (s.plan='unified' AND p.line='PRO')
WHERE s.state='active';
GRANT SELECT ON platform_access.subscription_terms TO platform_access,platform_jobs;

CREATE VIEW platform_manager.subscription_terms AS SELECT account_id,product_id,package_until FROM platform_access.subscription_terms;
GRANT SELECT ON platform_manager.subscription_terms TO platform_jobs;
ALTER TABLE platform_manager.account_change_events DROP CONSTRAINT account_change_events_kind_check,
    ADD CONSTRAINT account_change_events_kind_check CHECK (kind IN ('account_profile_updated','access_renewed','portrait_updated','subscription_granted','subscription_renewed','subscription_seats','subscription_revoked'));
