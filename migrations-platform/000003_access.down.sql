DROP VIEW platform_access.current_access;

REVOKE INSERT (account_id, product_id, valid_until, source, policy_version, policy_rule, policy_evidence, grant_event_id, granted_at)
    ON platform_lifecycle.terms FROM platform_jobs;
ALTER TABLE platform_lifecycle.terms
    DROP CONSTRAINT initial_term_evidence_complete,
    DROP COLUMN entitlement_version,
    DROP COLUMN policy_version,
    DROP COLUMN policy_rule,
    DROP COLUMN policy_evidence,
    DROP COLUMN grant_event_id,
    DROP COLUMN granted_at;

DROP TABLE platform_lifecycle.initial_term_policies;
