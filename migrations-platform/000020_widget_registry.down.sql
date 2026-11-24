ALTER TABLE platform_lifecycle.episodes DROP COLUMN installed_by;
REVOKE INSERT ON platform_lifecycle.scope FROM platform_jobs;
REVOKE INSERT ON platform_lifecycle.integrations FROM platform_jobs;
DROP TABLE platform_lifecycle.core_integrations;
