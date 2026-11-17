GRANT SELECT (account_id, product_id, valid_until, source, entitlement_version) ON platform_lifecycle.terms TO platform_jobs;
GRANT INSERT (reason, entitlement_version) ON platform_lifecycle.terms TO platform_jobs;
GRANT UPDATE (valid_until, source, reason, entitlement_version) ON platform_lifecycle.terms TO platform_jobs;
