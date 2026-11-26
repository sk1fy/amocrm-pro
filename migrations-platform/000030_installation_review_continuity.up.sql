CREATE FUNCTION platform_lifecycle.inherit_installation_review() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
    SELECT previous.reviewed_by, previous.reviewed_at INTO NEW.reviewed_by, NEW.reviewed_at
    FROM platform_lifecycle.episodes previous
    WHERE previous.installation_id = NEW.installation_id AND previous.generation < NEW.generation
      AND (previous.reviewed_by IS NOT NULL OR EXISTS (
          SELECT FROM platform_manager.review_events event WHERE event.episode_id = previous.id
      ))
    ORDER BY previous.generation DESC
    LIMIT 1;
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION platform_lifecycle.inherit_installation_review() FROM PUBLIC;
CREATE TRIGGER inherit_installation_review BEFORE INSERT ON platform_lifecycle.episodes
FOR EACH ROW WHEN (NEW.generation > 1 AND NEW.closed_at IS NULL AND NEW.reviewed_by IS NULL)
EXECUTE FUNCTION platform_lifecycle.inherit_installation_review();

WITH current_reinstalls AS (
    SELECT id, installation_id, generation
    FROM platform_lifecycle.episodes active_episode
    WHERE active_episode.closed_at IS NULL AND active_episode.generation > 1 AND active_episode.reviewed_by IS NULL
      AND NOT EXISTS (SELECT FROM platform_manager.review_events event WHERE event.episode_id = active_episode.id)
)
UPDATE platform_lifecycle.episodes active_episode
SET reviewed_by = prior.reviewed_by, reviewed_at = prior.reviewed_at, version = active_episode.version + 1
FROM current_reinstalls
CROSS JOIN LATERAL (
    SELECT previous.reviewed_by, previous.reviewed_at
    FROM platform_lifecycle.episodes previous
    WHERE previous.installation_id = current_reinstalls.installation_id AND previous.generation < current_reinstalls.generation
      AND (previous.reviewed_by IS NOT NULL OR EXISTS (
          SELECT FROM platform_manager.review_events event WHERE event.episode_id = previous.id
      ))
    ORDER BY previous.generation DESC
    LIMIT 1
) prior
WHERE active_episode.id = current_reinstalls.id AND prior.reviewed_by IS NOT NULL;
