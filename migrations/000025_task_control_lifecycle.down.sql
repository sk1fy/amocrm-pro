DROP TRIGGER task_control_integration_disabled ON integrations;
DROP FUNCTION task_control_integration_disabled();
DROP TRIGGER task_control_product_disabled ON integration_services;
DROP FUNCTION task_control_product_disabled();
DROP TRIGGER task_control_grant_revoked ON task_control_service_grants;
DROP FUNCTION task_control_grant_revoked();
DROP INDEX task_control_pending_checks;
ALTER TABLE task_control_installation_versions DROP COLUMN next_check_at, DROP COLUMN signal_pending, DROP COLUMN evidence;
