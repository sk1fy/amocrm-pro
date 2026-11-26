CREATE TABLE platform_manager.account_subscription_rosters (
    account_id UUID PRIMARY KEY REFERENCES platform_lifecycle.accounts(id),
    check_number BIGINT NOT NULL DEFAULT 0 CHECK (check_number >= 0),
    active_count INTEGER CHECK (active_count BETWEEN 0 AND 1000),
    observed_at TIMESTAMPTZ,
    previous_active_count INTEGER CHECK (previous_active_count BETWEEN 0 AND 1000),
    CHECK ((check_number = 0) = (active_count IS NULL)),
    CHECK ((active_count IS NULL) = (observed_at IS NULL)),
    CHECK ((check_number < 2) = (previous_active_count IS NULL))
);

GRANT SELECT, INSERT ON platform_manager.account_subscription_rosters TO platform_jobs;
GRANT UPDATE (check_number, active_count, observed_at, previous_active_count)
    ON platform_manager.account_subscription_rosters TO platform_jobs;

INSERT INTO platform_manager.account_subscription_rosters(account_id,check_number,active_count,observed_at)
SELECT account_id,1,active_count,observed_at FROM platform_manager.account_rosters WHERE observed_at IS NOT NULL;

ALTER TABLE platform_manager.account_subscription_revisions
    ADD COLUMN seats_checked_through BIGINT NOT NULL DEFAULT 0 CHECK (seats_checked_through >= 0);

CREATE OR REPLACE VIEW platform_manager.account_subscriptions AS
SELECT s.account_id,s.version,s.plan,s.ends_on,s.seats,s.deal_id,s.action,s.changed_at,
       (s.ends_on+1)::timestamp AT TIME ZONE 'Europe/Moscow' AS valid_until,
       CASE WHEN action='revoke' THEN 'revoked'
            WHEN ends_on < (statement_timestamp() AT TIME ZONE 'Europe/Moscow')::date THEN 'expired'
            WHEN r.active_count IS NULL THEN 'awaiting_users'
            WHEN checks.over_limit AND checks.previously_over_limit THEN 'suspended'
            ELSE 'active' END AS state,
       CASE WHEN action<>'revoke' AND ends_on >= (statement_timestamp() AT TIME ZONE 'Europe/Moscow')::date
                 AND checks.over_limit AND NOT checks.previously_over_limit THEN true END AS seats_warning
FROM (SELECT DISTINCT ON (account_id) account_id,version,plan,ends_on,seats,deal_id,action,changed_at,seats_checked_through
      FROM platform_manager.account_subscription_revisions ORDER BY account_id,version DESC) s
LEFT JOIN platform_manager.account_rosters r USING(account_id)
LEFT JOIN platform_manager.account_subscription_rosters c USING(account_id)
CROSS JOIN LATERAL (SELECT
    COALESCE(c.active_count > s.seats AND c.check_number > s.seats_checked_through,false) AS over_limit,
    COALESCE(c.previous_active_count > s.seats AND c.check_number - 1 > s.seats_checked_through,false) AS previously_over_limit) checks;
