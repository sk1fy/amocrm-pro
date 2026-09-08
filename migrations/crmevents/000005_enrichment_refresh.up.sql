-- Current objects and transient negative results must be claimable after TTL.
DROP INDEX event_enrichment_claim;
CREATE INDEX event_enrichment_claim ON event_enrichment_objects(installation_id, object_kind, run_after)
    WHERE state IN ('pending','retry')
       OR (state='ready' AND source<>'event_payload')
       OR (state='unavailable' AND reason_code IN ('not_found','permission_denied'));

-- Old event_payload rows were shared across events. Historical sidecars now
-- come from each crm_events envelope. Reload these shared rows only as current
-- upstream data, so reference-only events cannot inherit another event's text.
-- Also repair the placeholder produced by the former 32 KiB truncation.
UPDATE event_enrichment_objects
SET state=CASE WHEN object_kind='note' AND parent_type NOT IN ('leads','contacts','companies','customers') THEN 'unavailable' ELSE 'pending' END,
    reason_code=CASE WHEN object_kind='note' AND parent_type NOT IN ('leads','contacts','companies','customers') THEN 'unsupported' ELSE 'not_loaded' END,
    source='', payload='null'::jsonb, payload_hash=NULL, fetched_at=NULL,
    attempts=0, lease_token=lease_token+1, lease_until=NULL, run_after=now(), updated_at=now()
WHERE source='event_payload' OR (state='ready' AND payload='{"current":true}'::jsonb);
