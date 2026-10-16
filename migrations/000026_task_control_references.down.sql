DROP TRIGGER task_control_reference_client_changed ON integrations;
DROP FUNCTION task_control_reference_client_changed();
DROP TRIGGER task_control_reference_identity_changed ON installations;
DROP FUNCTION task_control_reference_identity_changed();
DROP TRIGGER task_control_grant_changed ON task_control_service_grants;
DROP FUNCTION task_control_grant_changed();
ALTER TABLE task_control_service_grants DROP COLUMN grant_revision, DROP COLUMN references_read;
