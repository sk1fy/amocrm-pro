DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM distribution_operations) THEN
  RAISE EXCEPTION 'distribution operations require explicit export/reconciliation before rollback';
 END IF;
END $$;
DROP TABLE distribution_control_receipts;
DROP TABLE distribution_result_outbox;
DROP TABLE distribution_operation_results;
DROP TABLE distribution_operation_observations;
DROP TABLE distribution_operation_attempts;
DROP TABLE distribution_lead_guards;
DROP TABLE distribution_operations;

DROP INDEX distribution_binding_operation_scope;
