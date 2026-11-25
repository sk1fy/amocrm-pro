CREATE TABLE platform_manager.installation_widget_versions (
    installation_id UUID NOT NULL,
    generation BIGINT NOT NULL CHECK (generation>=0),
    widget_version TEXT NOT NULL CHECK(char_length(widget_version) BETWEEN 1 AND 64),
    observed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(installation_id,generation)
);
GRANT SELECT, INSERT, UPDATE(widget_version,observed_at) ON platform_manager.installation_widget_versions TO platform_jobs;
