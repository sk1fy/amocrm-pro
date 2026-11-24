ALTER TABLE platform_lifecycle_integrations ADD COLUMN auto_enroll boolean NOT NULL DEFAULT false;
CREATE TABLE widget_activation_confirmations (
    installation_id uuid PRIMARY KEY REFERENCES installations(id),
    generation bigint NOT NULL CHECK (generation>0)
);

-- Internal consumers do not have a browser principal or a disposable token.
CREATE FUNCTION widget_work_enabled(installation uuid, integration uuid)
RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT NOT EXISTS (SELECT FROM platform_lifecycle_integrations m WHERE m.integration_id=$2)
        OR EXISTS (SELECT FROM widget_activation_confirmations a
                   JOIN platform_lifecycle_cursors c USING (installation_id)
                   JOIN installations i ON i.id=c.installation_id
                   WHERE i.id=$1 AND i.integration_id=$2 AND i.status='active'
                     AND a.generation=c.generation AND NOT c.closed)
$$;
