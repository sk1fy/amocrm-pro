CREATE TABLE platform_lifecycle.core_integrations (
    id uuid PRIMARY KEY,
    active boolean NOT NULL
);
GRANT SELECT, INSERT, UPDATE(active) ON platform_lifecycle.core_integrations TO platform_jobs;
GRANT INSERT ON platform_lifecycle.integrations TO platform_jobs;
GRANT INSERT ON platform_lifecycle.scope TO platform_jobs;
ALTER TABLE platform_lifecycle.episodes ADD COLUMN installed_by bigint CHECK (installed_by>0);
