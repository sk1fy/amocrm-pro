-- Existing installation.read grants never acquire reference access implicitly.
ALTER TABLE task_control_service_grants ADD COLUMN references_read boolean NOT NULL DEFAULT false;
ALTER TABLE task_control_service_grants ADD COLUMN grant_revision bigint NOT NULL DEFAULT 1 CHECK(grant_revision>0);
CREATE FUNCTION task_control_grant_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.grant_revision := OLD.grant_revision + 1;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_grant_changed BEFORE UPDATE ON task_control_service_grants
 FOR EACH ROW EXECUTE FUNCTION task_control_grant_changed();
-- OAuth/account identity changes fence even an A->B->A transition during a fetch.
CREATE FUNCTION task_control_reference_identity_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.account_domain IS DISTINCT FROM NEW.account_domain OR OLD.account_id IS DISTINCT FROM NEW.account_id OR OLD.integration_id IS DISTINCT FROM NEW.integration_id THEN
  UPDATE task_control_installation_versions SET generation=generation+1,revision=revision+1 WHERE installation_id=NEW.id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_reference_identity_changed AFTER UPDATE OF account_domain,account_id,integration_id ON installations
 FOR EACH ROW EXECUTE FUNCTION task_control_reference_identity_changed();
CREATE FUNCTION task_control_reference_client_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.client_id IS DISTINCT FROM NEW.client_id THEN
  UPDATE task_control_installation_versions v SET generation=generation+1,revision=revision+1
  FROM installations i WHERE v.installation_id=i.id AND i.integration_id=NEW.id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_reference_client_changed AFTER UPDATE OF client_id ON integrations
 FOR EACH ROW EXECUTE FUNCTION task_control_reference_client_changed();
