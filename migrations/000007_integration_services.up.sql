CREATE TABLE integration_services (
    integration_id UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
    service_code TEXT NOT NULL CHECK (service_code ~ '^[a-z][a-z0-9-]{0,63}$'),
    enabled BOOLEAN NOT NULL DEFAULT false,
    config JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (integration_id, service_code)
);

CREATE TRIGGER integration_services_updated_at
    BEFORE UPDATE ON integration_services
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Preserve the existing product contract exactly once during upgrade. New
-- integrations receive no implicit grants; provisioning must choose services.
INSERT INTO integration_services (integration_id, service_code, enabled)
SELECT id, 'lead-status', true FROM integrations;
