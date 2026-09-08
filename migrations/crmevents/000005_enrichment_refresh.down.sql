-- Repaired cache contents cannot be restored by rollback.
DROP INDEX event_enrichment_claim;
CREATE INDEX event_enrichment_claim ON event_enrichment_objects(installation_id, object_kind, run_after)
    WHERE state IN ('pending','retry');
