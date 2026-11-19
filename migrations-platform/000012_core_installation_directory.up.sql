CREATE TABLE platform_manager.observed_core_installations (
    installation_id uuid PRIMARY KEY,
    integration_id uuid NOT NULL,
    account_id uuid NOT NULL REFERENCES platform_lifecycle.accounts(id),
    product_id uuid NOT NULL REFERENCES platform_lifecycle.products(id),
    registered_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    UNIQUE (account_id, product_id),
    CHECK (registered_at <= observed_at)
);

CREATE VIEW platform_manager.observed_installation_directory AS
SELECT imported.*
FROM platform_manager.observed_core_installations imported
WHERE NOT EXISTS (
    SELECT FROM platform_lifecycle.account_products native
    WHERE native.installation_id = imported.installation_id
       OR (native.account_id, native.product_id) = (imported.account_id, imported.product_id)
);

CREATE VIEW platform_manager.installation_directory AS
SELECT * FROM platform_manager.installations
UNION ALL
SELECT observed.installation_id, observed.account_id, account.amo_account_id,
       observed.product_id, product.code, product.display_name, product.line,
       observed.installation_id, 0::bigint, observed.registered_at, NULL::timestamptz,
       1::bigint, NULL::uuid, NULL::timestamptz, 'observed', 'unknown', 'current',
       'unknown', NULL::timestamptz, NULL::text, 1::bigint
FROM platform_manager.observed_installation_directory observed
JOIN platform_lifecycle.accounts account ON account.id = observed.account_id
JOIN platform_lifecycle.products product ON product.id = observed.product_id
WHERE product.catalog_version IS NOT NULL;

CREATE VIEW platform_manager.account_product_directory AS
SELECT account_id, product_id, amo_account_id, current
FROM platform_manager.account_memberships
UNION ALL
SELECT d.account_id, d.product_id, a.amo_account_id, true
FROM platform_manager.observed_installation_directory d
JOIN platform_lifecycle.accounts a ON a.id = d.account_id
JOIN platform_lifecycle.products p ON p.id = d.product_id
WHERE p.catalog_version IS NOT NULL;

CREATE OR REPLACE VIEW platform_manager.visible_accounts AS
SELECT account_id, amo_account_id, count(*) FILTER (WHERE current)::integer AS active_products_count
FROM platform_manager.account_product_directory GROUP BY account_id, amo_account_id;

GRANT SELECT ON platform_manager.installation_directory, platform_manager.account_product_directory TO platform_jobs;
