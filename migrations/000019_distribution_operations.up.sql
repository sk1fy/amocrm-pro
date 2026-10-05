CREATE UNIQUE INDEX distribution_binding_operation_scope ON distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision);
CREATE TABLE distribution_operations (
 id UUID PRIMARY KEY,
 binding_id UUID NOT NULL REFERENCES distribution_bindings(id),
 company_id UUID NOT NULL,
 installation_id UUID NOT NULL,
 integration_id UUID NOT NULL,
 account_id BIGINT NOT NULL CHECK(account_id>0),
 binding_revision BIGINT NOT NULL CHECK(binding_revision BETWEEN 1 AND 9007199254740991),
 decision_id UUID NOT NULL,
 lead_id BIGINT NOT NULL CHECK(lead_id>0),
 key_hash BYTEA NOT NULL CHECK(octet_length(key_hash)=32),
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 command JSONB NOT NULL,
 receipt JSONB NOT NULL,
 job_id UUID NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','prechecking','applying','confirming','outcome_unknown','succeeded','no_change','rejected','conflict','cancelled')),
 external_effect_state TEXT NOT NULL DEFAULT 'no_attempt' CHECK(external_effect_state IN ('no_attempt','in_flight','unknown','settled')),
 result_version BIGINT NOT NULL DEFAULT 1 CHECK(result_version BETWEEN 1 AND 9007199254740991),
 outcome TEXT,
 evidence TEXT NOT NULL DEFAULT 'no_request_sent',
 error_code TEXT,
 confirmed_snapshot JSONB,
 cancel_requested_at TIMESTAMPTZ,
 finished_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(binding_id,key_hash), UNIQUE(company_id,decision_id),
 FOREIGN KEY(binding_id,company_id,installation_id,integration_id,account_id,binding_revision) REFERENCES distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision),
 FOREIGN KEY(installation_id,integration_id,account_id) REFERENCES installations(id,integration_id,account_id),
 FOREIGN KEY(job_id,installation_id) REFERENCES jobs(id,installation_id) ON DELETE RESTRICT,
 CHECK(finished_at IS NULL OR evidence IN ('no_request_sent','response_and_observation')),
 CHECK(finished_at IS NULL OR external_effect_state IN ('no_attempt','settled'))
);
CREATE TABLE distribution_lead_guards (
 account_id BIGINT NOT NULL, lead_id BIGINT NOT NULL,
 operation_id UUID NOT NULL UNIQUE REFERENCES distribution_operations(id),
 claimed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(account_id,lead_id)
);
CREATE TABLE distribution_operation_attempts (
 operation_id UUID PRIMARY KEY REFERENCES distribution_operations(id),
 job_id UUID NOT NULL, fence INTEGER NOT NULL CHECK(fence>0), executor TEXT NOT NULL,
 dispatch_started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 response_finished_at TIMESTAMPTZ,
 response_accepted BOOLEAN NOT NULL DEFAULT false,
 http_status INTEGER,
 transport_outcome TEXT NOT NULL DEFAULT 'ambiguous' CHECK(transport_outcome IN ('not_sent','response','ambiguous')),
 error_code TEXT,
 response_evidence JSONB
);
CREATE TABLE distribution_operation_observations (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(), operation_id UUID NOT NULL REFERENCES distribution_operations(id),
 snapshot JSONB NOT NULL, observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE distribution_operation_results (
 operation_id UUID NOT NULL REFERENCES distribution_operations(id), result_version BIGINT NOT NULL,
 payload JSONB NOT NULL, emitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(operation_id,result_version)
);
CREATE TABLE distribution_result_outbox (
 operation_id UUID NOT NULL, result_version BIGINT NOT NULL, payload JSONB NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','acknowledged')),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(operation_id,result_version),
 FOREIGN KEY(operation_id,result_version) REFERENCES distribution_operation_results(operation_id,result_version)
);
CREATE INDEX distribution_operations_recovery ON distribution_operations(state,updated_at) WHERE finished_at IS NULL;
CREATE TABLE distribution_control_receipts (
 binding_id UUID NOT NULL REFERENCES distribution_bindings(id),
 operation_id UUID NOT NULL REFERENCES distribution_operations(id),
 action TEXT NOT NULL CHECK(action IN ('cancel','reconcile')),
 key_hash BYTEA NOT NULL CHECK(octet_length(key_hash)=32),
 request_hash BYTEA NOT NULL CHECK(octet_length(request_hash)=32),
 response JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(binding_id,action,key_hash)
);
