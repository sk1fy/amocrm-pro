-- Infrastructure state for Task Control only; never stores business rules.
CREATE TABLE task_control_service_grants (
 key_id text NOT NULL, company_id uuid NOT NULL, account_id bigint NOT NULL CHECK(account_id>0),
 integration_id uuid NOT NULL REFERENCES integrations(id), enabled boolean NOT NULL DEFAULT false,
 PRIMARY KEY(key_id,company_id,account_id)
);
CREATE TABLE task_control_service_nonces (
 key_id text NOT NULL, nonce uuid NOT NULL, expires_at timestamptz NOT NULL,
 PRIMARY KEY(key_id,nonce)
);
CREATE TABLE task_control_installation_versions (
 installation_id uuid PRIMARY KEY REFERENCES installations(id), revision bigint NOT NULL DEFAULT 1,
 generation bigint NOT NULL DEFAULT 1, last_status text NOT NULL
);
CREATE FUNCTION task_control_installation_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM integration_services WHERE integration_id=NEW.integration_id AND service_code='task-control') THEN
  INSERT INTO task_control_installation_versions(installation_id,last_status) VALUES(NEW.id,NEW.status)
  ON CONFLICT(installation_id) DO UPDATE SET
   revision=task_control_installation_versions.revision+1,
   generation=task_control_installation_versions.generation+CASE WHEN NEW.status IN ('uninstalled','disabled') AND task_control_installation_versions.last_status NOT IN ('uninstalled','disabled') THEN 1 ELSE 0 END,
   last_status=NEW.status
  WHERE task_control_installation_versions.last_status<>NEW.status;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_control_installation_changed AFTER INSERT OR UPDATE OF status ON installations
 FOR EACH ROW EXECUTE FUNCTION task_control_installation_changed();
INSERT INTO task_control_installation_versions(installation_id,last_status)
 SELECT i.id,i.status FROM installations i JOIN integration_services p ON p.integration_id=i.integration_id AND p.service_code='task-control';
