CREATE TABLE platform_manager.installation_contacts (
    episode_id UUID PRIMARY KEY REFERENCES platform_lifecycle.episodes(id) ON DELETE CASCADE,
    amo_user_id BIGINT NOT NULL CHECK (amo_user_id > 0),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 512),
    email TEXT CHECK (length(email) <= 320),
    role TEXT CHECK (length(role) <= 256),
    phone TEXT NOT NULL CHECK (phone ~ '^\+[1-9][0-9]{7,14}$'),
    consent_version TEXT NOT NULL,
    activated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp()
);
GRANT SELECT, INSERT ON platform_manager.installation_contacts TO platform_jobs;

CREATE TABLE platform_manager.installation_deals (
    episode_id UUID PRIMARY KEY REFERENCES platform_lifecycle.episodes(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','reconciling','failed','skipped','succeeded')),
    title TEXT,
    company_account BIGINT,
    binding_revision TEXT,
    deal_id BIGINT CHECK (deal_id > 0),
    deal_host TEXT,
    reason TEXT,
    retry_version INTEGER NOT NULL DEFAULT 1 CHECK (retry_version > 0),
    CHECK (num_nonnulls(title,company_account,binding_revision) IN (0,3)),
    CHECK ((status='succeeded') = (num_nonnulls(deal_id,deal_host)=2)),
    CHECK (status<>'reconciling' OR title IS NOT NULL)
);
GRANT SELECT, UPDATE ON platform_manager.installation_deals TO platform_jobs;

CREATE FUNCTION platform_manager.schedule_installation_deal() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
    INSERT INTO platform_manager.installation_deals(episode_id) VALUES(NEW.episode_id);
    INSERT INTO platform_jobs.jobs(id,kind,payload,available_at)
    VALUES(NEW.episode_id,'installation.deal',jsonb_build_object('episode_id',NEW.episode_id),
           GREATEST(NEW.occurred_at + INTERVAL '24 hours',statement_timestamp()));
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION platform_manager.schedule_installation_deal() FROM PUBLIC;

CREATE TRIGGER schedule_installation_deal AFTER INSERT ON platform_lifecycle.history
FOR EACH ROW WHEN (NEW.kind IN ('installed','reinstalled'))
EXECUTE FUNCTION platform_manager.schedule_installation_deal();

CREATE TABLE platform_manager.installation_deal_retries (
    id UUID PRIMARY KEY,
    subject_id UUID NOT NULL,
    idempotency_key UUID NOT NULL,
    episode_id UUID NOT NULL REFERENCES platform_manager.installation_deals(episode_id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    UNIQUE(subject_id,idempotency_key),
    UNIQUE(episode_id,version)
);
GRANT SELECT, INSERT ON platform_manager.installation_deal_retries TO platform_jobs;
