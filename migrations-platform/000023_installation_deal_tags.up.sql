-- The deal row serializes the first successful write and reauthorization.
CREATE FUNCTION platform_manager.queue_installation_deal_tags() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE episode uuid := NEW.episode_id;
        deal_status text;
BEGIN
    IF TG_TABLE_SCHEMA = 'platform_lifecycle' THEN
        SELECT status INTO deal_status FROM platform_manager.installation_deals
        WHERE episode_id=episode FOR UPDATE;
        IF deal_status IS DISTINCT FROM 'succeeded' THEN RETURN NEW; END IF;
    END IF;
    INSERT INTO platform_jobs.jobs(id,kind,payload)
    VALUES(gen_random_uuid(),'installation.deal.tags',jsonb_build_object('episode_id',episode));
    RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION platform_manager.queue_installation_deal_tags() FROM PUBLIC;
CREATE TRIGGER queue_tags_on_deal AFTER UPDATE OF status ON platform_manager.installation_deals
FOR EACH ROW WHEN (NEW.status='succeeded' AND OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION platform_manager.queue_installation_deal_tags();
CREATE TRIGGER queue_tags_on_reauthorization AFTER INSERT ON platform_lifecycle.history
FOR EACH ROW WHEN (NEW.kind='reauthorized') EXECUTE FUNCTION platform_manager.queue_installation_deal_tags();

INSERT INTO platform_jobs.jobs(id,kind,payload)
SELECT gen_random_uuid(),'installation.deal.tags',jsonb_build_object('episode_id',d.episode_id)
FROM platform_manager.installation_deals d WHERE d.status='succeeded';
