ALTER TABLE platform_lifecycle.products
    ADD COLUMN trial_extension_days integer NOT NULL DEFAULT 7 CHECK (trial_extension_days BETWEEN 1 AND 365),
    ADD COLUMN tariff_options jsonb NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(tariff_options) = 'array' AND jsonb_array_length(tariff_options) <= 50);
GRANT UPDATE (tariff_options,trial_extension_days) ON platform_lifecycle.products TO platform_jobs;

-- A catalog manager selecting FREE supplies the evidence for this rule only.
CREATE FUNCTION platform_manager.record_free_category(product uuid, actor uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
    INSERT INTO platform_lifecycle.verified_trial_policies(product_id,version,rule,evidence)
    SELECT product, COALESCE(max(version),0)+1, 'free', 'FREE category selected by ' || actor::text
    FROM platform_lifecycle.verified_trial_policies WHERE product_id=product
    HAVING count(*) FILTER (WHERE rule='free') = 0
$$;
REVOKE ALL ON FUNCTION platform_manager.record_free_category(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_manager.record_free_category(uuid,uuid) TO platform_jobs;
