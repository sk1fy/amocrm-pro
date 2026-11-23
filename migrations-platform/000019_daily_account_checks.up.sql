ALTER TABLE platform_manager.account_refresh_slots
    DROP CONSTRAINT account_refresh_slots_component_check,
    ADD CHECK (component IN ('users','license','installations')),
    ADD COLUMN next_check_at timestamptz NOT NULL DEFAULT statement_timestamp(),
    ADD COLUMN attempted_at timestamptz,
    ADD COLUMN succeeded_at timestamptz,
    ADD COLUMN result_code text;
GRANT UPDATE (next_check_at, attempted_at, succeeded_at, result_code)
    ON platform_manager.account_refresh_slots TO platform_jobs;

CREATE TABLE platform_manager.core_inventory (
    installation_id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES platform_lifecycle.accounts(id),
    integration_id uuid NOT NULL,
    product_id uuid NOT NULL REFERENCES platform_lifecycle.products(id),
    registered_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    current boolean NOT NULL,
    lifecycle_sequence bigint NOT NULL CHECK (lifecycle_sequence>=0),
    observation_only boolean NOT NULL,
    auth_state text NOT NULL CHECK (auth_state IN ('authorized','disabled','reauth_required','unknown'))
);
CREATE INDEX core_inventory_account ON platform_manager.core_inventory(account_id);
GRANT SELECT, INSERT, DELETE ON platform_manager.core_inventory TO platform_jobs;

CREATE OR REPLACE VIEW platform_manager.observed_installation_directory AS
SELECT imported.*
FROM (SELECT installation_id,integration_id,account_id,product_id,registered_at,observed_at FROM platform_manager.core_inventory
    UNION ALL SELECT o.* FROM platform_manager.observed_core_installations o
    WHERE NOT EXISTS (SELECT FROM platform_manager.core_inventory c WHERE c.installation_id=o.installation_id)) imported
WHERE NOT EXISTS (
    SELECT FROM platform_lifecycle.account_products native
    WHERE native.installation_id = imported.installation_id
       OR ((native.account_id, native.product_id) = (imported.account_id, imported.product_id) AND NOT EXISTS
           (SELECT FROM platform_manager.core_inventory c WHERE c.installation_id=imported.installation_id))
);

CREATE OR REPLACE VIEW platform_manager.installation_directory AS
SELECT e.id,e.account_id,e.amo_account_id,e.product_id,e.product_code,e.product_name,e.line,
 e.installation_id,e.generation,e.opened_at,e.closed_at,e.version,e.reviewed_by,e.reviewed_at,e.kind,e.last_lifecycle_event,
 CASE WHEN c.current=false THEN 'removed' ELSE e.lifecycle END AS lifecycle,
 COALESCE(c.auth_state,e.authorization) AS authorization,e.valid_until,e.term_source,e.entitlement_version
FROM platform_manager.installations e
JOIN platform_lifecycle.cursors k ON k.installation_id=e.installation_id
LEFT JOIN platform_manager.core_inventory c ON c.installation_id=e.installation_id AND e.generation=k.generation
 AND e.lifecycle='current' AND c.lifecycle_sequence>=k.sequence
UNION ALL
SELECT observed.installation_id, observed.account_id, account.amo_account_id,
       observed.product_id, product.code, product.display_name, product.line,
       observed.installation_id, 0::bigint, observed.registered_at, NULL::timestamptz,
       1::bigint, NULL::uuid, NULL::timestamptz, 'observed', 'unknown', CASE WHEN c.current=false THEN 'removed' ELSE 'current' END,
       COALESCE(c.auth_state,'unknown'), NULL::timestamptz, NULL::text, 1::bigint
FROM platform_manager.observed_installation_directory observed
LEFT JOIN platform_manager.core_inventory c ON c.installation_id=observed.installation_id
JOIN platform_lifecycle.accounts account ON account.id = observed.account_id
JOIN platform_lifecycle.products product ON product.id = observed.product_id
WHERE product.catalog_version IS NOT NULL;

CREATE OR REPLACE VIEW platform_manager.account_product_directory AS
SELECT m.account_id,m.product_id,m.amo_account_id,(m.current AND COALESCE(c.current,true)) AS current
FROM platform_manager.account_memberships m JOIN platform_lifecycle.cursors k USING(installation_id)
LEFT JOIN platform_manager.core_inventory c ON c.installation_id=m.installation_id AND c.lifecycle_sequence>=k.sequence
UNION ALL
SELECT d.account_id, d.product_id, a.amo_account_id, COALESCE(c.current,true)
FROM platform_manager.observed_installation_directory d
LEFT JOIN platform_manager.core_inventory c ON c.installation_id=d.installation_id
JOIN platform_lifecycle.accounts a ON a.id = d.account_id
JOIN platform_lifecycle.products p ON p.id = d.product_id
WHERE p.catalog_version IS NOT NULL;

GRANT UPDATE (observed_at,current,auth_state,observation_only,lifecycle_sequence) ON platform_manager.core_inventory TO platform_jobs;

CREATE VIEW platform_manager.account_read_sources AS
SELECT m.account_id,m.amo_account_id,m.installation_id,m.integration_id,m.generation,m.authorization_sequence,false AS observation_only
FROM platform_manager.account_memberships m
JOIN platform_lifecycle.cursors k USING(installation_id)
LEFT JOIN platform_manager.core_inventory c ON c.installation_id=m.installation_id AND c.lifecycle_sequence>=k.sequence
WHERE COALESCE(c.current AND c.auth_state='authorized',true) AND m.current AND m.effective_authorization='authorized' AND m.generation>0 AND m.authorization_sequence>0
UNION ALL
SELECT i.account_id,a.amo_account_id,i.installation_id,i.integration_id,0::bigint,0::bigint,true
FROM platform_manager.core_inventory i JOIN platform_lifecycle.accounts a ON a.id=i.account_id
WHERE i.current AND i.auth_state='authorized' AND i.observation_only
  AND NOT EXISTS (SELECT FROM platform_lifecycle.account_products p WHERE p.installation_id=i.installation_id);
GRANT SELECT ON platform_manager.account_read_sources TO platform_jobs;
GRANT SELECT (account_id, installation_id) ON platform_manager.observed_core_installations TO platform_jobs;

CREATE OR REPLACE VIEW platform_manager.visible_accounts AS
SELECT account_id, amo_account_id, count(DISTINCT product_id) FILTER (WHERE current)::integer AS active_products_count
FROM platform_manager.account_product_directory GROUP BY account_id, amo_account_id;
