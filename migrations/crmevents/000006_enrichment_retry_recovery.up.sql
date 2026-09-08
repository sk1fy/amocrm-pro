-- Resume legacy transient failures, including batches rejected by old Gateway.
UPDATE event_enrichment_objects
SET state='retry', attempts=0, lease_token=lease_token+1, lease_until=NULL,
    run_after=now(), updated_at=now()
WHERE state='error' AND reason_code='temporary';
