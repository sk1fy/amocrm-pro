DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM distribution_admin_pauses WHERE paused) THEN
  RAISE EXCEPTION 'Refusing rollback while an installation is paused; explicit resume required';
 END IF;
END $$;
DROP INDEX distribution_admin_operations_scope;
DROP INDEX distribution_admin_events_scope;
DROP TRIGGER installation_distribution_admin_pause ON installations;
DROP FUNCTION initialize_distribution_admin_pause();
DROP TABLE distribution_admin_pauses;
