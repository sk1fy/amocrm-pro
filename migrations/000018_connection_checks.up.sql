CREATE TABLE installation_checks (
    installation_id uuid PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    receipt_id uuid NOT NULL REFERENCES admin_commands(id),
    credential_version bigint NOT NULL,
    installation_status text NOT NULL CHECK (installation_status IN ('active','pending','authorizing','reauth_required','error')),
    classification text NOT NULL CHECK (classification IN ('verified_ok','auth_error','network_error','rate_limited','internal_error')),
    observed_at timestamptz NOT NULL,
    retry_after bigint NOT NULL DEFAULT 0 CHECK (retry_after >= 0),
    failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0),
    next_check_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX installation_checks_due_idx ON installation_checks(next_check_at);
CREATE TRIGGER installation_checks_updated BEFORE UPDATE ON installation_checks
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- Session advisory locks enforce process-wide and integration-wide request
-- budgets. A small durable reservation spreads new accounts after restarts.
CREATE TABLE installation_check_schedule (
    installation_id uuid PRIMARY KEY REFERENCES installations(id) ON DELETE CASCADE,
    next_check_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX installation_check_schedule_due_idx ON installation_check_schedule(next_check_at);
CREATE TRIGGER installation_check_schedule_updated BEFORE UPDATE ON installation_check_schedule
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
