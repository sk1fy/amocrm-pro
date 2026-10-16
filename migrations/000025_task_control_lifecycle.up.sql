ALTER TABLE task_control_installation_versions
 ADD COLUMN evidence text NOT NULL DEFAULT 'registry',
 ADD COLUMN signal_pending boolean NOT NULL DEFAULT false,
 ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX task_control_pending_checks ON task_control_installation_versions(next_check_at) WHERE signal_pending;

CREATE FUNCTION task_control_grant_revoked() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.enabled AND (TG_OP='DELETE' OR NOT NEW.enabled OR NEW.integration_id<>OLD.integration_id) THEN
  UPDATE task_control_installation_versions v SET generation=generation+1,revision=revision+1
  FROM installations i WHERE v.installation_id=i.id AND i.integration_id=OLD.integration_id AND i.account_id=OLD.account_id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_grant_revoked AFTER UPDATE OR DELETE ON task_control_service_grants FOR EACH ROW EXECUTE FUNCTION task_control_grant_revoked();

CREATE FUNCTION task_control_product_disabled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.service_code='task-control' AND OLD.enabled AND (TG_OP='DELETE' OR NOT NEW.enabled) THEN
  UPDATE task_control_installation_versions v SET generation=generation+1,revision=revision+1
  FROM installations i WHERE v.installation_id=i.id AND i.integration_id=OLD.integration_id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_product_disabled AFTER UPDATE OR DELETE ON integration_services FOR EACH ROW EXECUTE FUNCTION task_control_product_disabled();

CREATE FUNCTION task_control_integration_disabled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status='active' AND NEW.status<>'active' THEN
  UPDATE task_control_installation_versions v SET generation=generation+1,revision=revision+1
  FROM installations i WHERE v.installation_id=i.id AND i.integration_id=NEW.id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_integration_disabled AFTER UPDATE OF status ON integrations FOR EACH ROW EXECUTE FUNCTION task_control_integration_disabled();
