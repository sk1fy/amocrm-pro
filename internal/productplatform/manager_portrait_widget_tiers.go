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
	current_products AS (
		SELECT product_id, line, "authorization", valid_until, term_source
		FROM platform_manager.installation_directory
		WHERE account_id=a.account_id AND lifecycle='current'
	),
	working AS (
		SELECT DISTINCT product_id FROM current_products
		WHERE "authorization"='authorized'
		  AND valid_until > statement_timestamp()
		  AND term_source IN ('own_trial', 'own_paid', 'manual', 'free')
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
