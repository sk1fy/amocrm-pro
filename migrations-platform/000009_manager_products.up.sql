CREATE TABLE platform_lifecycle.verified_tariff_versions (
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    evidence TEXT NOT NULL CHECK (char_length(btrim(evidence)) BETWEEN 1 AND 1000),
    PRIMARY KEY (product_id,version)
);
CREATE TABLE platform_lifecycle.verified_trial_policies (
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    rule TEXT NOT NULL CHECK (rule IN ('free','top_90d','pro_7d','pro_14d')),
    evidence TEXT NOT NULL CHECK (char_length(btrim(evidence)) BETWEEN 1 AND 256),
    PRIMARY KEY (product_id,version),
    UNIQUE (product_id,version,rule,evidence)
);
CREATE TABLE platform_lifecycle.product_trial_revisions (
    product_id UUID NOT NULL REFERENCES platform_lifecycle.products(id),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    rule TEXT NOT NULL CHECK (rule IN ('none','free','top_90d','pro_7d','pro_14d')),
    source_version BIGINT,
    evidence TEXT,
    actor_id UUID,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (product_id,version),
    FOREIGN KEY (product_id,source_version,rule,evidence)
        REFERENCES platform_lifecycle.verified_trial_policies(product_id,version,rule,evidence),
    CHECK ((rule='none' AND num_nonnulls(source_version,evidence)=0)
        OR (rule<>'none' AND num_nonnulls(source_version,evidence)=2))
);
INSERT INTO platform_lifecycle.verified_trial_policies(product_id,version,rule,evidence)
SELECT product_id,version,rule,evidence FROM platform_lifecycle.initial_term_policies;
INSERT INTO platform_lifecycle.product_trial_revisions(product_id,version,rule,source_version,evidence)
SELECT product_id,version,rule,version,evidence FROM platform_lifecycle.initial_term_policies;

ALTER TABLE platform_lifecycle.products
    ADD COLUMN catalog_version BIGINT CHECK (catalog_version BETWEEN 1 AND 9007199254740991),
    ADD COLUMN availability TEXT CHECK (availability IN ('draft','pilot','active','archived')),
    ADD COLUMN links JSONB,
    ADD COLUMN tariff_version BIGINT,
    ADD COLUMN catalog_policy_version BIGINT,
    ADD CONSTRAINT product_tariff_evidence FOREIGN KEY (id,tariff_version)
        REFERENCES platform_lifecycle.verified_tariff_versions(product_id,version),
    ADD CONSTRAINT product_trial_revision FOREIGN KEY (id,catalog_policy_version)
        REFERENCES platform_lifecycle.product_trial_revisions(product_id,version),
    ADD CONSTRAINT product_catalog_complete CHECK (
        (num_nonnulls(catalog_version,availability,links,catalog_policy_version)=0 AND tariff_version IS NULL)
        OR (num_nonnulls(catalog_version,availability,links,catalog_policy_version,display_name,line)=6
            AND char_length(btrim(display_name)) BETWEEN 1 AND 512
            AND jsonb_typeof(links)='array' AND jsonb_array_length(links)<=20));

CREATE FUNCTION platform_lifecycle.require_catalog_policy() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE target UUID;
BEGIN
    IF TG_TABLE_NAME='products' THEN target=NEW.id;
    ELSE target=COALESCE(NEW.product_id,OLD.product_id);
    END IF;
    IF EXISTS (
        SELECT FROM platform_lifecycle.products p
        JOIN platform_lifecycle.product_trial_revisions r ON r.product_id=p.id AND r.version=p.catalog_policy_version
        LEFT JOIN platform_lifecycle.initial_term_policies live ON live.product_id=p.id
        WHERE p.id=target AND p.catalog_version IS NOT NULL AND (
            (r.rule='none' AND live.product_id IS NOT NULL)
            OR (r.rule<>'none' AND (live.rule,live.version,live.evidence) IS DISTINCT FROM (r.rule,r.version,r.evidence))
            OR (p.line='FREE' AND r.rule NOT IN ('none','free'))
            OR (p.line='TOP' AND r.rule NOT IN ('none','top_90d'))
            OR (p.line='PRO' AND r.rule NOT IN ('none','pro_7d','pro_14d'))
        )
    ) THEN RAISE EXCEPTION 'adopted product policy must match its verified catalog revision'
        USING ERRCODE='check_violation';
    END IF;
    RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION platform_lifecycle.require_catalog_policy() FROM PUBLIC;
CREATE CONSTRAINT TRIGGER require_product_catalog_policy AFTER INSERT OR UPDATE ON platform_lifecycle.products
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION platform_lifecycle.require_catalog_policy();
CREATE CONSTRAINT TRIGGER require_live_catalog_policy AFTER INSERT OR UPDATE OR DELETE ON platform_lifecycle.initial_term_policies
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION platform_lifecycle.require_catalog_policy();

CREATE TABLE platform_manager.product_catalog_revision (
    id BOOLEAN PRIMARY KEY CHECK(id),
    version BIGINT NOT NULL DEFAULT 1 CHECK(version BETWEEN 1 AND 9007199254740991)
);
INSERT INTO platform_manager.product_catalog_revision VALUES(true);
CREATE FUNCTION platform_manager.advance_product_catalog() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    UPDATE platform_manager.product_catalog_revision SET version=version+1 WHERE id;
    RETURN NULL;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.advance_product_catalog() FROM PUBLIC;
CREATE TRIGGER catalog_products_revision AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON platform_lifecycle.products
    FOR EACH STATEMENT EXECUTE FUNCTION platform_manager.advance_product_catalog();
CREATE TRIGGER catalog_integrations_revision AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON platform_lifecycle.integrations
    FOR EACH STATEMENT EXECUTE FUNCTION platform_manager.advance_product_catalog();
CREATE TRIGGER catalog_policy_revision AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON platform_lifecycle.initial_term_policies
    FOR EACH STATEMENT EXECUTE FUNCTION platform_manager.advance_product_catalog();
CREATE TRIGGER catalog_trial_history_revision AFTER INSERT OR UPDATE OR DELETE OR TRUNCATE ON platform_lifecycle.product_trial_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION platform_manager.advance_product_catalog();

ALTER TABLE platform_manager.commands
    DROP CONSTRAINT commands_target_shape,
    ADD CONSTRAINT commands_target_shape CHECK (
        (kind='installation.review' AND episode_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(account_id,product_id,resource_id)=0)
        OR (kind='account.refresh' AND account_id IS NOT NULL AND num_nonnulls(episode_id,expected_version,product_id,resource_id)=0)
        OR (kind IN ('account.profile','account.portrait','account.portrait_audit_date')
            AND account_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,product_id,resource_id)=0)
        OR (kind='account.renewal' AND num_nonnulls(account_id,product_id,expected_version)=3 AND num_nonnulls(episode_id,resource_id)=0)
        OR (kind='portrait.template' AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,resource_id)=0)
        OR (kind='portrait.niche.create' AND num_nonnulls(episode_id,account_id,product_id,resource_id,expected_version)=0)
        OR (kind IN ('portrait.niche.update','portrait.niche.delete','portrait.niche.merge')
            AND resource_id IS NOT NULL AND expected_version IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id)=0)
        OR (kind='product.create' AND resource_id IS NOT NULL AND num_nonnulls(episode_id,account_id,product_id,expected_version)=0)
        OR (kind='product.update' AND product_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id,account_id,resource_id)=0));

GRANT SELECT ON platform_lifecycle.products, platform_lifecycle.verified_tariff_versions,
    platform_lifecycle.verified_trial_policies, platform_lifecycle.product_trial_revisions TO platform_jobs;
GRANT INSERT (id,code) ON platform_lifecycle.products TO platform_jobs;
GRANT UPDATE (display_name,line,catalog_version,availability,links,tariff_version,catalog_policy_version)
    ON platform_lifecycle.products TO platform_jobs;
GRANT INSERT ON platform_lifecycle.product_trial_revisions TO platform_jobs;
GRANT SELECT, INSERT, UPDATE, DELETE ON platform_lifecycle.initial_term_policies TO platform_jobs;
GRANT SELECT ON platform_manager.product_catalog_revision TO platform_jobs;
CREATE VIEW platform_manager.product_access AS
SELECT product_id,account_id,authorized,source,own_until FROM platform_access.current_access;
GRANT SELECT ON platform_manager.product_access TO platform_jobs;

CREATE CONSTRAINT TRIGGER require_history_catalog_policy AFTER INSERT OR UPDATE OR DELETE ON platform_lifecycle.product_trial_revisions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION platform_lifecycle.require_catalog_policy();

CREATE FUNCTION platform_manager.adopt_trial_evidence(product UUID) RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    IF EXISTS(SELECT FROM platform_lifecycle.products WHERE id=product AND catalog_version IS NULL) THEN
        INSERT INTO platform_lifecycle.verified_trial_policies(product_id,version,rule,evidence)
        SELECT product_id,version,rule,evidence FROM platform_lifecycle.initial_term_policies WHERE product_id=product
        ON CONFLICT DO NOTHING;
        IF EXISTS(
            SELECT FROM platform_lifecycle.initial_term_policies p
            LEFT JOIN platform_lifecycle.verified_trial_policies v ON (v.product_id,v.version,v.rule,v.evidence)=(p.product_id,p.version,p.rule,p.evidence)
            WHERE p.product_id=product AND v.product_id IS NULL
        ) THEN RAISE EXCEPTION 'active trial conflicts with prior verified evidence' USING ERRCODE='check_violation';
        END IF;
        INSERT INTO platform_lifecycle.product_trial_revisions(product_id,version,rule,source_version,evidence)
        SELECT product_id,version,rule,version,evidence FROM platform_lifecycle.initial_term_policies WHERE product_id=product
        ON CONFLICT DO NOTHING;
    END IF;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.adopt_trial_evidence(UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_manager.adopt_trial_evidence(UUID) TO platform_jobs;

CREATE FUNCTION platform_manager.apply_catalog_policy(product UUID, revision BIGINT) RETURNS VOID
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE selected TEXT;
BEGIN
    SELECT r.rule INTO STRICT selected FROM platform_lifecycle.product_trial_revisions r
    JOIN platform_lifecycle.products p ON p.id=r.product_id AND p.catalog_policy_version=r.version
    WHERE r.product_id=product AND r.version=revision AND p.catalog_version IS NOT NULL;
    IF selected='none' THEN
        DELETE FROM platform_lifecycle.initial_term_policies WHERE product_id=product;
    ELSE
        INSERT INTO platform_lifecycle.initial_term_policies(product_id,rule,version,evidence)
        SELECT product_id,rule,version,evidence FROM platform_lifecycle.product_trial_revisions WHERE product_id=product AND version=revision
        ON CONFLICT(product_id) DO UPDATE SET rule=EXCLUDED.rule,version=EXCLUDED.version,evidence=EXCLUDED.evidence;
    END IF;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.apply_catalog_policy(UUID, BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_manager.apply_catalog_policy(UUID, BIGINT) TO platform_jobs;
REVOKE INSERT,UPDATE,DELETE ON platform_lifecycle.initial_term_policies FROM platform_jobs;
