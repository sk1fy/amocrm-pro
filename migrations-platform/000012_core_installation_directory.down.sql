CREATE OR REPLACE VIEW platform_manager.visible_accounts AS
SELECT account_id, amo_account_id, count(*) FILTER (WHERE current)::integer AS active_products_count
FROM platform_manager.account_memberships GROUP BY account_id, amo_account_id;

DROP VIEW platform_manager.account_product_directory;
DROP VIEW platform_manager.installation_directory;
DROP VIEW platform_manager.observed_installation_directory;
DROP TABLE platform_manager.observed_core_installations;
