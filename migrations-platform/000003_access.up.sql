CREATE TABLE platform_lifecycle.initial_term_policies (
    product_id UUID PRIMARY KEY REFERENCES platform_lifecycle.products(id),
    rule TEXT NOT NULL CHECK (rule IN ('free', 'top_90d', 'pro_7d', 'pro_14d')),
    version BIGINT NOT NULL CHECK (version BETWEEN 1 AND 9007199254740991),
    evidence TEXT NOT NULL CHECK (length(btrim(evidence)) BETWEEN 1 AND 256)
);

ALTER TABLE platform_lifecycle.terms
    ADD COLUMN entitlement_version BIGINT NOT NULL DEFAULT 1 CHECK (entitlement_version BETWEEN 1 AND 9007199254740991),
    ADD COLUMN policy_version BIGINT CHECK (policy_version BETWEEN 1 AND 9007199254740991),
    ADD COLUMN policy_rule TEXT,
    ADD COLUMN policy_evidence TEXT,
    ADD COLUMN grant_event_id UUID REFERENCES platform_lifecycle.inbox(event_id),
    ADD COLUMN granted_at TIMESTAMPTZ,
    ADD CONSTRAINT initial_term_evidence_complete CHECK (
        num_nonnulls(policy_version, policy_rule, policy_evidence, grant_event_id, granted_at) IN (0, 5)
    );

GRANT SELECT ON platform_lifecycle.initial_term_policies TO platform_jobs;
GRANT INSERT (account_id, product_id, valid_until, source, policy_version, policy_rule, policy_evidence, grant_event_id, granted_at)
    ON platform_lifecycle.terms TO platform_jobs;

CREATE VIEW platform_access.current_access AS
SELECT a.amo_account_id, catalog.code AS product_code, pair.account_id, pair.product_id,
       pair.integration_id, pair.installation_id, cursor.generation, cursor.authorization_sequence,
       (episode.id IS NOT NULL AND cursor.effective_authorization = 'authorized'
        AND EXISTS (
            SELECT FROM platform_lifecycle.integrations i JOIN platform_lifecycle.scope s ON s.integration_id=i.id
            WHERE i.id=pair.integration_id AND i.product_id=pair.product_id AND s.amo_account_id=a.amo_account_id)
        AND NOT EXISTS (
            SELECT FROM platform_lifecycle.integrations i JOIN platform_lifecycle.scope s ON s.integration_id=i.id
            WHERE i.id<>pair.integration_id AND i.product_id=pair.product_id AND s.amo_account_id=a.amo_account_id)
       ) AS authorized,
       terms.valid_until AS own_until, terms.source, COALESCE(terms.entitlement_version, 1) AS entitlement_version
FROM platform_lifecycle.account_products pair
JOIN platform_lifecycle.accounts a ON a.id=pair.account_id
JOIN platform_lifecycle.products catalog ON catalog.id=pair.product_id
JOIN platform_lifecycle.cursors cursor ON cursor.installation_id=pair.installation_id
LEFT JOIN platform_lifecycle.episodes episode ON episode.installation_id=pair.installation_id
    AND episode.generation=cursor.generation AND episode.closed_at IS NULL
LEFT JOIN platform_lifecycle.terms terms USING (account_id, product_id);

GRANT SELECT ON platform_access.current_access TO platform_access;
