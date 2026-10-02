-- Connection data may not be silently destroyed by a normal rollback.
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM distribution_bindings) THEN
  RAISE EXCEPTION 'distribution bindings require explicit export/reconciliation before rollback';
 END IF;
END $$;
DROP TABLE distribution_binding_tombstones;
DROP TABLE distribution_actor_mappings;
DROP TABLE distribution_bindings;
DROP TABLE distribution_service_nonces;
DROP TABLE distribution_service_grants;

DROP INDEX installation_distribution_scope;
