package productplatform

type portraitWidgetTiers struct {
	FreeInstalled int                `json:"free_installed"`
	Top           portraitWidgetTier `json:"top"`
	Pro           portraitWidgetTier `json:"pro"`
}

type portraitWidgetTier struct {
	Working int `json:"working"`
	Total   int `json:"total"`
}

const portraitWidgetTiersJSON = `(
	WITH catalog AS (
		SELECT id, line FROM platform_lifecycle.products
		WHERE availability IN ('active', 'pilot') AND line IN ('TOP', 'PRO')
	),
	covered AS MATERIALIZED (
        SELECT product_id,package_until FROM platform_manager.subscription_terms WHERE account_id=$1
    ),
    current_products AS (
		SELECT product_id, line, "authorization", valid_until, term_source, package_until
		FROM platform_manager.installation_directory LEFT JOIN covered USING(product_id)
		WHERE account_id=$1 AND lifecycle='current'
	),
	working AS (
		SELECT DISTINCT product_id FROM current_products
		WHERE "authorization"='authorized'
		  AND (package_until > statement_timestamp() OR (valid_until > statement_timestamp()
		  AND term_source IN ('own_trial', 'own_paid', 'manual', 'free')))
	)
	SELECT jsonb_build_object(
		'free_installed', (SELECT count(DISTINCT product_id) FROM current_products WHERE line='FREE'),
		'top', jsonb_build_object(
			'working', count(working.product_id) FILTER (WHERE catalog.line='TOP'),
			'total', count(*) FILTER (WHERE catalog.line='TOP')
		),
		'pro', jsonb_build_object(
			'working', count(working.product_id) FILTER (WHERE catalog.line='PRO'),
			'total', count(*) FILTER (WHERE catalog.line='PRO')
		)
	)
	FROM catalog LEFT JOIN working ON working.product_id=catalog.id
)`
