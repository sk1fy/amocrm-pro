#!/bin/sh
# Run on the operator host. Both endpoints may use PGSERVICE credentials.
set -eu
case "${1:---report}" in --report|--apply) ;; *) echo 'usage: account-observation-bindings.sh [--report|--apply]' >&2; exit 2;; esac
: "${PLATFORM_DSN:?set PLATFORM_DSN}" "${CORE_DSN:?set CORE_DSN}"
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
psql -Xq -v ON_ERROR_STOP=1 "$PLATFORM_DSN" -c "COPY (
  SELECT o.installation_id,o.integration_id,a.amo_account_id,p.code
  FROM platform_manager.observed_core_installations o
  JOIN platform_lifecycle.accounts a ON a.id=o.account_id
  JOIN platform_lifecycle.products p ON p.id=o.product_id
  WHERE p.catalog_version IS NOT NULL ORDER BY o.installation_id
) TO STDOUT" > "$work/bindings"
{
cat <<'SQL'
BEGIN;
CREATE TEMP TABLE expected (installation_id uuid PRIMARY KEY,integration_id uuid,amo_account_id bigint,product_code text) ON COMMIT DROP;
COPY expected FROM STDIN;
SQL
cat "$work/bindings"
printf '%s\n' '\.'
cat <<'SQL'
SELECT i.account_id,n.code AS integration,e.product_code,
 CASE WHEN m.product_code IS NOT NULL THEN 'lifecycle'
      WHEN e.product_code IS NULL THEN 'needs_catalog_review'
      WHEN b.installation_id IS NULL THEN 'confirmed_import'
      ELSE 'already_registered' END AS result
FROM installations i JOIN integrations n ON n.id=i.integration_id
LEFT JOIN expected e ON (e.installation_id,e.integration_id,e.amo_account_id)=(i.id,i.integration_id,i.account_id)
LEFT JOIN platform_lifecycle_scope s ON s.integration_id=i.integration_id AND s.account_id=i.account_id
LEFT JOIN platform_lifecycle_integrations m ON m.integration_id=s.integration_id
LEFT JOIN platform_observation_bindings b ON b.installation_id=i.id
WHERE i.account_id IN (SELECT amo_account_id FROM expected UNION SELECT account_id FROM platform_lifecycle_scope)
ORDER BY i.account_id,n.code;
DO $check$ BEGIN
 IF EXISTS(SELECT FROM expected e LEFT JOIN installations i ON (i.id,i.integration_id,i.account_id)=(e.installation_id,e.integration_id,e.amo_account_id)
 LEFT JOIN platform_observation_bindings b ON b.installation_id=e.installation_id
 LEFT JOIN platform_lifecycle_integrations m ON m.integration_id=e.integration_id
 WHERE i.id IS NULL OR (b.installation_id IS NOT NULL AND (b.product_code,b.integration_id,b.amo_account_id) IS DISTINCT FROM (e.product_code,e.integration_id,e.amo_account_id))
 OR (m.product_code IS NOT NULL AND m.product_code<>e.product_code))
 THEN RAISE EXCEPTION 'catalog binding conflict'; END IF;
END $check$;
SQL
if [ "${1:---report}" = --apply ]; then
cat <<'SQL'
INSERT INTO platform_observation_bindings(installation_id,integration_id,amo_account_id,product_code)
SELECT e.* FROM expected e WHERE NOT EXISTS(SELECT FROM platform_lifecycle_scope s WHERE s.integration_id=e.integration_id AND s.account_id=e.amo_account_id)
ON CONFLICT (installation_id) DO NOTHING;
COMMIT;
SQL
else echo 'ROLLBACK;'; fi
} > "$work/import.sql"
psql -X -v ON_ERROR_STOP=1 "$CORE_DSN" -f "$work/import.sql"
