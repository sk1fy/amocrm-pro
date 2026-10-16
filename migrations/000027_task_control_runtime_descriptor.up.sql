-- Operator-confirmed package evidence is scoped to an installation generation.
-- Browser version claims can negotiate a protocol but cannot enable it.
CREATE TABLE task_control_runtime_descriptors (
 installation_id uuid NOT NULL REFERENCES installations(id) ON DELETE CASCADE,
 generation bigint NOT NULL CHECK(generation>0),
 runtime_version text NOT NULL CHECK(runtime_version IN ('1.1.4','1.2.0')),
 confirmed_at timestamptz NOT NULL DEFAULT now(),
 confirmed_by text NOT NULL CHECK(length(confirmed_by) BETWEEN 1 AND 200),
 PRIMARY KEY(installation_id,generation)
);
