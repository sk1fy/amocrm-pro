CREATE ROLE platform_owner LOGIN PASSWORD 'platform_owner_dev' CONNECTION LIMIT 4;
CREATE ROLE platform_access LOGIN PASSWORD 'platform_access_dev' CONNECTION LIMIT 20;
CREATE ROLE platform_jobs LOGIN PASSWORD 'platform_jobs_dev' CONNECTION LIMIT 20;
CREATE DATABASE amocrm_platform OWNER platform_owner;
REVOKE ALL ON DATABASE amocrm_platform FROM PUBLIC;
GRANT CONNECT ON DATABASE amocrm_platform TO platform_access, platform_jobs;

REVOKE CONNECT ON DATABASE postgres, template1 FROM PUBLIC;

\connect amocrm_platform
REVOKE ALL ON SCHEMA public FROM PUBLIC;
