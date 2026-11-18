CREATE SCHEMA platform_company;
CREATE TABLE platform_company.source_scopes (
 id UUID PRIMARY KEY, account_id BIGINT NOT NULL CHECK(account_id>0),
 binding_revision TEXT NOT NULL CHECK(length(binding_revision) BETWEEN 1 AND 256),
 UNIQUE(account_id,binding_revision), UNIQUE(id,account_id)
);
CREATE TABLE platform_company.source_control (
 singleton BOOLEAN PRIMARY KEY CHECK(singleton), scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 epoch BIGINT NOT NULL CHECK(epoch>0), retry_not_before TIMESTAMPTZ
);
CREATE TABLE platform_company.generations (
 id UUID PRIMARY KEY, scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 component TEXT NOT NULL CHECK(component IN ('clients','metadata')),
 observed_at TIMESTAMPTZ NOT NULL, fresh_until TIMESTAMPTZ NOT NULL,
 source_state TEXT NOT NULL CHECK(source_state IN ('fresh','stale')),
 content_hash BYTEA NOT NULL CHECK(octet_length(content_hash)=32),
 required_fields_state TEXT,
 CHECK((component='metadata' AND required_fields_state IS NOT NULL) OR (component='clients' AND required_fields_state IS NULL)),
 UNIQUE(scope_id,component,observed_at), UNIQUE(id,scope_id,component), UNIQUE(id,component)
);
CREATE TABLE platform_company.source_components (
 scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 component TEXT NOT NULL CHECK(component IN ('clients','metadata')), current_generation UUID,
 attempt_state TEXT NOT NULL DEFAULT 'disabled' CHECK(attempt_state IN ('fresh','stale','error','disabled')),
 attempt_at TIMESTAMPTZ, error_code TEXT, next_refresh TIMESTAMPTZ,
 current_job_id UUID REFERENCES platform_jobs.jobs,
 PRIMARY KEY(scope_id,component),
 FOREIGN KEY(current_generation,scope_id,component) REFERENCES platform_company.generations(id,scope_id,component)
);
CREATE TABLE platform_company.company_identities (
 id UUID PRIMARY KEY, source_account_id BIGINT NOT NULL CHECK(source_account_id>0), crm_company_id BIGINT NOT NULL CHECK(crm_company_id>0),
 UNIQUE(source_account_id,crm_company_id), UNIQUE(id,crm_company_id), UNIQUE(id,crm_company_id,source_account_id)
);
CREATE TABLE platform_company.company_rows (
 generation_id UUID NOT NULL, component TEXT NOT NULL DEFAULT 'clients' CHECK(component='clients'),
 company_id UUID NOT NULL, crm_company_id BIGINT NOT NULL, scope_id UUID NOT NULL, source_account_id BIGINT NOT NULL,
 name TEXT NOT NULL, source_resolution TEXT NOT NULL,
 PRIMARY KEY(generation_id,company_id), UNIQUE(generation_id,company_id,crm_company_id),
 FOREIGN KEY(generation_id,component) REFERENCES platform_company.generations(id,component),
 FOREIGN KEY(company_id,crm_company_id,source_account_id) REFERENCES platform_company.company_identities(id,crm_company_id,source_account_id),
 FOREIGN KEY(scope_id,source_account_id) REFERENCES platform_company.source_scopes(id,account_id),
 FOREIGN KEY(generation_id,scope_id,component) REFERENCES platform_company.generations(id,scope_id,component)
);
CREATE TABLE platform_company.company_account_claims (
 generation_id UUID NOT NULL, company_id UUID NOT NULL, account_id BIGINT NOT NULL CHECK(account_id>0),
 PRIMARY KEY(generation_id,company_id,account_id),
 FOREIGN KEY(generation_id,company_id) REFERENCES platform_company.company_rows
);
CREATE INDEX company_claim_lookup ON platform_company.company_account_claims(generation_id,account_id);
CREATE TABLE platform_company.contact_rows (
 generation_id UUID NOT NULL, component TEXT NOT NULL DEFAULT 'clients' CHECK(component='clients'),
 crm_contact_id BIGINT NOT NULL CHECK(crm_contact_id>0), name TEXT NOT NULL,
 PRIMARY KEY(generation_id,crm_contact_id),
 FOREIGN KEY(generation_id,component) REFERENCES platform_company.generations(id,component)
);
CREATE TABLE platform_company.contact_company_links (
 generation_id UUID NOT NULL, crm_contact_id BIGINT NOT NULL,
 crm_company_id BIGINT NOT NULL CHECK(crm_company_id>0), company_id UUID,
 PRIMARY KEY(generation_id,crm_contact_id,crm_company_id),
 FOREIGN KEY(generation_id,crm_contact_id) REFERENCES platform_company.contact_rows,
 FOREIGN KEY(generation_id,company_id,crm_company_id) REFERENCES platform_company.company_rows(generation_id,company_id,crm_company_id)
);
CREATE TABLE platform_company.workflow_pipelines (
 generation_id UUID NOT NULL, component TEXT NOT NULL DEFAULT 'metadata' CHECK(component='metadata'),
 pipeline_id BIGINT NOT NULL CHECK(pipeline_id>0), name TEXT NOT NULL,
 PRIMARY KEY(generation_id,pipeline_id),
 FOREIGN KEY(generation_id,component) REFERENCES platform_company.generations(id,component)
);
CREATE TABLE platform_company.workflow_stages (
 generation_id UUID NOT NULL, pipeline_id BIGINT NOT NULL, stage_id BIGINT NOT NULL CHECK(stage_id>0), name TEXT NOT NULL,
 PRIMARY KEY(generation_id,pipeline_id,stage_id),
 FOREIGN KEY(generation_id,pipeline_id) REFERENCES platform_company.workflow_pipelines
);
CREATE TABLE platform_company.product_workflow_revisions (
 product_id UUID NOT NULL REFERENCES platform_lifecycle.products,
 revision BIGINT NOT NULL CHECK(revision BETWEEN 1 AND 9007199254740990),
 scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 metadata_generation UUID NOT NULL, component TEXT NOT NULL DEFAULT 'metadata' CHECK(component='metadata'),
 pipeline_id BIGINT NOT NULL, stage_id BIGINT NOT NULL,
 command_id UUID NOT NULL UNIQUE REFERENCES platform_manager.commands,
 actor_id UUID NOT NULL, validated_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 PRIMARY KEY(product_id,revision),
 FOREIGN KEY(metadata_generation,scope_id,component) REFERENCES platform_company.generations(id,scope_id,component),
 FOREIGN KEY(metadata_generation,pipeline_id,stage_id) REFERENCES platform_company.workflow_stages
);
CREATE TABLE platform_company.product_workflow_heads (
 product_id UUID PRIMARY KEY REFERENCES platform_lifecycle.products,
 version BIGINT NOT NULL CHECK(version BETWEEN 2 AND 9007199254740991),
 revision BIGINT NOT NULL CHECK(revision=version-1),
 FOREIGN KEY(product_id,revision) REFERENCES platform_company.product_workflow_revisions
);
CREATE TABLE platform_company.audit (
 id UUID PRIMARY KEY, kind TEXT NOT NULL CHECK(kind IN ('source.activated','product.company_workflow.set')),
 scope_id UUID NOT NULL REFERENCES platform_company.source_scopes,
 epoch BIGINT, previous_scope UUID REFERENCES platform_company.source_scopes, previous_epoch BIGINT,
 product_id UUID, revision BIGINT, command_id UUID REFERENCES platform_manager.commands,
 actor_id UUID NOT NULL, recorded_at TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
 FOREIGN KEY(product_id,revision) REFERENCES platform_company.product_workflow_revisions,
 CHECK((kind='source.activated' AND num_nonnulls(epoch,previous_scope,previous_epoch)=3 AND epoch>0 AND previous_epoch>0 AND num_nonnulls(product_id,revision,command_id)=0)
 OR (kind='product.company_workflow.set' AND num_nonnulls(product_id,revision,command_id)=3 AND num_nonnulls(epoch,previous_scope,previous_epoch)=0))
);

CREATE FUNCTION platform_company.register_source(account BIGINT, binding TEXT)
RETURNS TABLE(scope_id UUID, epoch BIGINT)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE target UUID;
BEGIN
 PERFORM pg_advisory_xact_lock(1579107215,12);
 IF EXISTS(SELECT FROM platform_company.source_control) THEN
   RETURN QUERY SELECT c.scope_id,c.epoch FROM platform_company.source_control c
   JOIN platform_company.source_scopes s ON s.id=c.scope_id WHERE s.account_id=account AND s.binding_revision=binding;
   RETURN;
 END IF;
 target:=gen_random_uuid();
 INSERT INTO platform_company.source_scopes VALUES(target,account,binding);
 INSERT INTO platform_company.source_control VALUES(true,target,1,NULL);
 INSERT INTO platform_company.source_components(scope_id,component) VALUES(target,'clients'),(target,'metadata');
 RETURN QUERY SELECT target,1::bigint;
END $$;
REVOKE ALL ON FUNCTION platform_company.register_source(BIGINT,TEXT) FROM PUBLIC;

CREATE FUNCTION platform_company.activate_source(expected_scope UUID, expected_epoch BIGINT, account BIGINT, binding TEXT, actor UUID)
RETURNS TABLE(scope_id UUID, epoch BIGINT)
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE old_scope UUID; old_epoch BIGINT; target UUID;
BEGIN
 PERFORM pg_advisory_xact_lock(1579107215,12);
 SELECT c.scope_id,c.epoch INTO STRICT old_scope,old_epoch FROM platform_company.source_control c FOR UPDATE;
 IF old_scope IS DISTINCT FROM expected_scope OR old_epoch IS DISTINCT FROM expected_epoch THEN
  RAISE EXCEPTION 'source activation conflict' USING ERRCODE='serialization_failure';
 END IF;
 INSERT INTO platform_company.source_scopes VALUES(gen_random_uuid(),account,binding) ON CONFLICT(account_id,binding_revision) DO NOTHING;
 SELECT s.id INTO STRICT target FROM platform_company.source_scopes s WHERE s.account_id=account AND s.binding_revision=binding;
 INSERT INTO platform_company.source_components(scope_id,component) VALUES(target,'clients'),(target,'metadata') ON CONFLICT DO NOTHING;
 UPDATE platform_company.source_control SET scope_id=target,epoch=old_epoch+1,retry_not_before=NULL;
 UPDATE platform_company.source_components SET current_generation=NULL,current_job_id=NULL,attempt_state='disabled',attempt_at=NULL,error_code=NULL,next_refresh=NULL WHERE source_components.scope_id=target;
 INSERT INTO platform_company.audit(id,kind,scope_id,epoch,previous_scope,previous_epoch,actor_id)
 VALUES(gen_random_uuid(),'source.activated',target,old_epoch+1,old_scope,old_epoch,actor);
 RETURN QUERY SELECT target,old_epoch+1;
END $$;
REVOKE ALL ON FUNCTION platform_company.activate_source(UUID,BIGINT,BIGINT,TEXT,UUID) FROM PUBLIC;

GRANT USAGE ON SCHEMA platform_company TO platform_jobs;
GRANT SELECT ON ALL TABLES IN SCHEMA platform_company TO platform_jobs;
GRANT EXECUTE ON FUNCTION platform_company.register_source(BIGINT,TEXT) TO platform_jobs;
GRANT INSERT ON platform_company.generations,platform_company.company_identities,platform_company.company_rows,
 platform_company.company_account_claims,platform_company.contact_rows,platform_company.contact_company_links,
 platform_company.workflow_pipelines,platform_company.workflow_stages,platform_company.product_workflow_heads,
 platform_company.product_workflow_revisions TO platform_jobs;
GRANT UPDATE(current_generation,attempt_state,attempt_at,error_code,next_refresh,current_job_id) ON platform_company.source_components TO platform_jobs;
GRANT UPDATE(retry_not_before) ON platform_company.source_control TO platform_jobs;
GRANT UPDATE(version,revision) ON platform_company.product_workflow_heads TO platform_jobs;

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
        OR (kind IN ('product.update','product.company_workflow.set') AND product_id IS NOT NULL AND expected_version IS NOT NULL
            AND num_nonnulls(episode_id,account_id,resource_id)=0));

CREATE FUNCTION platform_company.enqueue_refresh(scope UUID, activation BIGINT, part TEXT, job UUID, job_kind TEXT)
RETURNS VOID LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF job_kind <> 'company.refresh.' || scope::text || '.' || activation::text OR activation<1 OR part NOT IN ('clients','metadata') THEN
  RAISE EXCEPTION 'invalid company refresh job' USING ERRCODE='check_violation';
 END IF;
 PERFORM FROM platform_company.source_control WHERE scope_id=scope AND epoch=activation
  AND (retry_not_before IS NULL OR retry_not_before<=statement_timestamp()) FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'source activation changed' USING ERRCODE='serialization_failure'; END IF;
 PERFORM FROM platform_company.source_components c LEFT JOIN platform_jobs.jobs j ON j.id=c.current_job_id
  WHERE c.scope_id=scope AND c.component=part AND (j.id IS NULL OR j.status IN ('succeeded','dead'))
  AND (c.next_refresh IS NULL OR c.next_refresh<=statement_timestamp()) FOR UPDATE OF c;
 IF NOT FOUND THEN RAISE EXCEPTION 'company refresh already scheduled' USING ERRCODE='serialization_failure'; END IF;
 INSERT INTO platform_jobs.jobs(id,kind,payload) VALUES(job,job_kind,jsonb_build_object('scope_id',scope,'epoch',activation,'component',part));
 UPDATE platform_company.source_components SET current_job_id=job WHERE scope_id=scope AND component=part;
END $$;
REVOKE ALL ON FUNCTION platform_company.enqueue_refresh(UUID,BIGINT,TEXT,UUID,TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform_company.enqueue_refresh(UUID,BIGINT,TEXT,UUID,TEXT) TO platform_jobs;

CREATE FUNCTION platform_company.audit_workflow_revision() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 INSERT INTO platform_company.audit(id,kind,scope_id,product_id,revision,command_id,actor_id)
 VALUES(gen_random_uuid(),'product.company_workflow.set',NEW.scope_id,NEW.product_id,NEW.revision,NEW.command_id,NEW.actor_id);
 RETURN NULL;
END $$;
REVOKE ALL ON FUNCTION platform_company.audit_workflow_revision() FROM PUBLIC;
CREATE TRIGGER audit_workflow_revision AFTER INSERT ON platform_company.product_workflow_revisions
 FOR EACH ROW EXECUTE FUNCTION platform_company.audit_workflow_revision();
