CREATE TABLE activity_problem_sources (
 installation_id UUID PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
 integration_id UUID NOT NULL REFERENCES integrations(id),
 current_generation UUID,
 staging_generation UUID NOT NULL,
 phase TEXT NOT NULL DEFAULT 'leads' CHECK(phase IN ('leads','tasks','idle')),
 next_page INTEGER NOT NULL DEFAULT 1 CHECK(next_page>0),
 snapshot_at TIMESTAMPTZ,
 next_poll_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 requested_until TIMESTAMPTZ NOT NULL,
 lease_token BIGINT NOT NULL DEFAULT 0,
 lease_until TIMESTAMPTZ,
 error_code TEXT NOT NULL DEFAULT ''
);
CREATE INDEX activity_problem_sources_due ON activity_problem_sources(next_poll_at);
CREATE TABLE activity_problem_leads (
 installation_id UUID NOT NULL REFERENCES activity_problem_sources(installation_id) ON DELETE CASCADE,
 generation UUID NOT NULL,
 id BIGINT NOT NULL,
 responsible_user_id BIGINT NOT NULL,
 pipeline_id BIGINT NOT NULL,
 status_id BIGINT NOT NULL,
 is_deleted BOOLEAN NOT NULL,
 PRIMARY KEY(installation_id,generation,id)
);
CREATE INDEX activity_problem_leads_manager ON activity_problem_leads(installation_id,generation,responsible_user_id,pipeline_id);
CREATE TABLE activity_problem_tasks (
 installation_id UUID NOT NULL REFERENCES activity_problem_sources(installation_id) ON DELETE CASCADE,
 generation UUID NOT NULL,
 id BIGINT NOT NULL,
 responsible_user_id BIGINT NOT NULL,
 entity_id BIGINT NOT NULL,
 entity_type TEXT NOT NULL,
 complete_till BIGINT NOT NULL,
 is_completed BOOLEAN NOT NULL,
 PRIMARY KEY(installation_id,generation,id)
);
CREATE INDEX activity_problem_tasks_entity ON activity_problem_tasks(installation_id,generation,entity_type,entity_id);
CREATE INDEX activity_problem_tasks_manager ON activity_problem_tasks(installation_id,generation,responsible_user_id);
