ALTER TABLE activity_command_receipts
    DROP CONSTRAINT activity_command_receipts_actor_id_nonnegative,
    ADD CONSTRAINT activity_command_receipts_actor_id_check CHECK (actor_id > 0);

ALTER TABLE lead_status_workflow_rule_configurations
    DROP CONSTRAINT lead_status_rule_configurations_positive,
    ADD CONSTRAINT lead_status_rule_configurations_positive CHECK (
        actor_user_id > 0 AND source_pipeline_id > 0 AND source_status_id > 0
        AND target_pipeline_id > 0 AND target_status_id > 0 AND revision > 0
    );
