DROP TABLE IF EXISTS lead_status_workflow_rule_configurations;

ALTER TABLE lead_status_workflow_rules
    DROP CONSTRAINT IF EXISTS lead_status_workflow_rules_revision_positive,
    DROP COLUMN IF EXISTS revision;
