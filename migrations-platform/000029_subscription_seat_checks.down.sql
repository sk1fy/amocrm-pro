DROP VIEW platform_manager.subscription_terms;
DROP VIEW platform_access.subscription_terms;
DROP VIEW platform_manager.account_subscriptions;

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

ALTER TABLE platform_manager.account_subscription_revisions DROP COLUMN seats_checked_through;

DROP TABLE platform_manager.account_subscription_rosters;
