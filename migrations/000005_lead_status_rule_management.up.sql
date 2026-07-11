ALTER TABLE lead_status_workflow_rules
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1,
    ADD CONSTRAINT lead_status_workflow_rules_revision_positive CHECK (revision > 0);

CREATE TABLE lead_status_workflow_rule_configurations (
    job_id UUID PRIMARY KEY,
    installation_id UUID NOT NULL,
    rule_id UUID NOT NULL,
    actor_user_id BIGINT NOT NULL,
    source_pipeline_id BIGINT NOT NULL,
    source_status_id BIGINT NOT NULL,
    target_pipeline_id BIGINT NOT NULL,
    target_status_id BIGINT NOT NULL,
    enabled BOOLEAN NOT NULL,
    revision BIGINT NOT NULL,
    configured_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT lead_status_rule_configurations_job_installation_fkey
        FOREIGN KEY (job_id, installation_id)
        REFERENCES jobs(id, installation_id) ON DELETE RESTRICT,
    CONSTRAINT lead_status_rule_configurations_rule_installation_fkey
        FOREIGN KEY (rule_id, installation_id)
        REFERENCES lead_status_workflow_rules(id, installation_id) ON DELETE RESTRICT,
    CONSTRAINT lead_status_rule_configurations_rule_revision_key
        UNIQUE (rule_id, revision),
    CONSTRAINT lead_status_rule_configurations_positive CHECK (
        actor_user_id > 0 AND source_pipeline_id > 0 AND source_status_id > 0
        AND target_pipeline_id > 0 AND target_status_id > 0 AND revision > 0
    ),
    CONSTRAINT lead_status_rule_configurations_changes_state CHECK (
        source_pipeline_id <> target_pipeline_id
        OR source_status_id <> target_status_id
    )
);
