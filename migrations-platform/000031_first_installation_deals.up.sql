DROP TRIGGER schedule_installation_deal ON platform_lifecycle.history;

CREATE TRIGGER schedule_installation_deal AFTER INSERT ON platform_lifecycle.history
FOR EACH ROW WHEN (NEW.kind = 'installed')
EXECUTE FUNCTION platform_manager.schedule_installation_deal();

-- Lock jobs before deals, as queue completion and manual retry do. A reserved
-- deal may already exist in CRM, so leave it available for reconciliation.
WITH reinstall_jobs AS MATERIALIZED (
    SELECT job.id
    FROM platform_jobs.jobs job
    JOIN platform_lifecycle.episodes episode ON episode.id = job.id
    WHERE job.kind = 'installation.deal' AND episode.generation > 1
      AND job.status IN ('queued', 'running', 'dead')
    ORDER BY job.id
    FOR UPDATE OF job
), skipped AS (
    UPDATE platform_manager.installation_deals deal
    SET status = 'skipped', reason = NULL
    FROM reinstall_jobs job
    WHERE deal.episode_id = job.id AND deal.status IN ('pending', 'failed') AND deal.title IS NULL
    RETURNING deal.episode_id
)
UPDATE platform_jobs.jobs job
SET status = 'succeeded', lease_until = NULL, failure_code = NULL, updated_at = statement_timestamp()
FROM skipped
WHERE job.id = skipped.episode_id;
