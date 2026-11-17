REVOKE UPDATE (valid_until, source, reason, entitlement_version) ON platform_lifecycle.terms FROM platform_jobs;
REVOKE INSERT (reason, entitlement_version) ON platform_lifecycle.terms FROM platform_jobs;
REVOKE SELECT (account_id, product_id, valid_until, source, entitlement_version) ON platform_lifecycle.terms FROM platform_jobs;
