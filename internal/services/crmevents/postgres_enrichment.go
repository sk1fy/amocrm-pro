package crmevents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Postgres) enqueueEnrichment(ctx context.Context, tx pgx.Tx, installation uuid.UUID, events []serviceapi.Event) error {
	if len(events) == 0 {
		return nil
	}
	type link struct {
		eventID string
		object  plannedObject
	}
	var links []link
	for _, event := range events {
		if event.ID == "" {
			continue
		}
		for _, object := range planEnrichment(event) {
			if object.Source == serviceapi.SourceEventPayload {
				continue
			}
			links = append(links, link{eventID: event.ID, object: object})
		}
		for _, object := range s.notesForReadyTask(ctx, tx, installation, event) {
			links = append(links, link{eventID: event.ID, object: object})
		}
	}
	if len(links) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	seen := map[string]bool{}
	for _, item := range links {
		k := item.object.Kind + "\x00" + item.object.Key
		if !seen[k] {
			seen[k] = true
			batch.Queue(`INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,parent_type,parent_id,object_id,state,reason_code,source,payload,payload_hash,fetched_at,run_after,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CASE WHEN $7='ready' THEN now() ELSE NULL END,now(),now()) ON CONFLICT DO NOTHING`, installation, item.object.Kind, item.object.Key, item.object.ParentType, item.object.ParentID, item.object.ObjectID, item.object.State, item.object.Reason, item.object.Source, jsonbOrNull(item.object.Payload), payloadHash(item.object.Payload))
		}
		batch.Queue(`INSERT INTO event_enrichment_links(installation_id,event_id,object_kind,object_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, installation, item.eventID, item.object.Kind, item.object.Key)
	}
	return tx.SendBatch(ctx, batch).Close()
}

func (s *Postgres) notesForReadyTask(ctx context.Context, tx pgx.Tx, installation uuid.UUID, event serviceapi.Event) []plannedObject {
	if event.Type != "task_result_added" || event.EntityID <= 0 {
		return nil
	}
	var state, parentType string
	var parentID int64
	var payload json.RawMessage
	err := tx.QueryRow(ctx, `SELECT state,parent_type,parent_id,payload FROM event_enrichment_objects WHERE installation_id=$1 AND object_kind=$2 AND object_key=$3`, installation, serviceapi.ObjectTask, numericKey(event.EntityID)).Scan(&state, &parentType, &parentID, &payload)
	if err != nil {
		return nil
	}
	if state != serviceapi.EnrichmentReady {
		return nil
	}
	if parentType == "" || parentID == 0 {
		var task serviceapi.Task
		_ = json.Unmarshal(payload, &task)
		parentType, parentID = task.EntityType, task.EntityID
	}
	object, ok := planNoteAfterTask(event, parentType, parentID)
	if !ok {
		return nil
	}
	return []plannedObject{object}
}

// Only transient negative results expire. Unsupported/invalid results stay terminal; transient
// failures remain retryable; event_payload is immutable history, not a current cache.
const enrichmentClaimable = `(o.state IN ('pending','retry') OR (o.state='ready' AND o.source<>'event_payload') OR (o.state='unavailable' AND o.reason_code IN ('not_found','permission_denied')))`

func (s *Postgres) ClaimEnrichment(ctx context.Context) (EnrichmentClaim, error) {
	var c EnrichmentClaim
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT o.installation_id,s.integration_id,o.object_kind,o.parent_type FROM event_enrichment_objects o JOIN event_sources s ON s.installation_id=o.installation_id WHERE `+enrichmentClaimable+` AND o.run_after<=now() AND (o.lease_until IS NULL OR o.lease_until<now()) AND s.state NOT IN ('reauth_required','disabled','paused','failed') AND EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=o.installation_id AND c.enabled) AND NOT EXISTS(SELECT 1 FROM event_jobs j JOIN event_sources src ON src.installation_id=j.installation_id WHERE j.installation_id=o.installation_id AND j.kind IN ('current','backfill') AND j.status='running' AND src.lease_until>now()) AND NOT EXISTS(SELECT 1 FROM event_enrichment_objects busy WHERE busy.installation_id=o.installation_id AND busy.lease_until>now()) ORDER BY o.run_after,o.object_kind,o.object_key FOR UPDATE OF o SKIP LOCKED LIMIT 1`).Scan(&c.InstallationID, &c.IntegrationID, &c.Kind, &c.ParentType)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNoWork
	}
	if err != nil {
		return c, err
	}
	// Serialize claim admission per account, without taking the source row or
	// collector lease. Recheck after acquiring: the initial snapshot may have
	// preceded another transaction's commit.
	var admitted bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('crm-events/enrichment/' || $1::text,0))`, c.InstallationID.String()).Scan(&admitted); err != nil {
		return c, err
	}
	if !admitted {
		return c, ErrNoWork
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_enrichment_objects WHERE installation_id=$1 AND lease_until>now())`, c.InstallationID).Scan(&busy); err != nil {
		return c, err
	}
	if busy {
		return c, ErrNoWork
	}
	rows, err := tx.Query(ctx, `UPDATE event_enrichment_objects o SET attempts=CASE WHEN o.state='retry' THEN o.attempts ELSE 0 END,lease_token=lease_token+1,lease_until=now()+$4*interval '1 millisecond',updated_at=now() FROM (SELECT o.object_key FROM event_enrichment_objects o WHERE o.installation_id=$1 AND o.object_kind=$2 AND o.parent_type=$3 AND `+enrichmentClaimable+` AND o.run_after<=now() AND (o.lease_until IS NULL OR o.lease_until<now()) ORDER BY o.run_after,o.object_key FOR UPDATE OF o SKIP LOCKED LIMIT $5) picked WHERE o.installation_id=$1 AND o.object_kind=$2 AND o.object_key=picked.object_key RETURNING o.object_key,o.parent_type,o.parent_id,o.object_id,o.lease_token,o.attempts`, c.InstallationID, c.Kind, c.ParentType, s.cfg.Lease.Milliseconds(), serviceapi.EnrichmentBatchLimit)
	if err != nil {
		return c, err
	}
	for rows.Next() {
		var o claimedObject
		if err = rows.Scan(&o.Key, &o.ParentType, &o.ParentID, &o.ObjectID, &o.Token, &o.Attempts); err != nil {
			rows.Close()
			return c, err
		}
		c.Objects = append(c.Objects, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, err
	}
	if len(c.Objects) == 0 {
		return c, ErrNoWork
	}
	c.ID = uuid.New()
	return c, tx.Commit(ctx)
}

func (s *Postgres) SaveEnrichment(ctx context.Context, c EnrichmentClaim, results []enrichmentSave) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	byKey := map[string]claimedObject{}
	for _, o := range c.Objects {
		byKey[o.Key] = o
	}
	for _, result := range results {
		claimed, ok := byKey[result.Key]
		if !ok {
			continue
		}
		if len(result.Payload) > maxEnrichmentObjectBytes {
			result.State, result.Reason = serviceapi.EnrichmentError, serviceapi.ReasonInvalid
			result.Payload = nil
		}
		if result.State == serviceapi.EnrichmentReady {
			result.Delay = enrichmentTTL(c.Kind)
		}
		var fetched any
		if !result.FetchedAt.IsZero() {
			fetched = result.FetchedAt.UTC()
		}
		tag, err := tx.Exec(ctx, `UPDATE event_enrichment_objects SET state=$4,reason_code=$5,source=$6,payload=$7,payload_hash=$8,fetched_at=COALESCE($9::timestamptz,fetched_at),attempts=0,lease_until=NULL,run_after=now()+$10*interval '1 millisecond',parent_type=CASE WHEN $11<>'' THEN $11 ELSE parent_type END,parent_id=CASE WHEN $12>0 THEN $12 ELSE parent_id END,updated_at=now() WHERE installation_id=$1 AND object_kind=$2 AND object_key=$3 AND lease_token=$13 AND lease_until>now()`, c.InstallationID, c.Kind, result.Key, result.State, result.Reason, result.Source, jsonbOrNull(result.Payload), payloadHash(result.Payload), fetched, result.Delay.Milliseconds(), result.ParentType, result.ParentID, claimed.Token)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		if c.Kind == serviceapi.ObjectTask && result.State == serviceapi.EnrichmentReady {
			if err = s.enqueueNotesForReadyTask(ctx, tx, c.InstallationID, result); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Postgres) enqueueNotesForReadyTask(ctx context.Context, tx pgx.Tx, installation uuid.UUID, result enrichmentSave) error {
	parentType, parentID := result.ParentType, result.ParentID
	if result.Task != nil {
		if parentType == "" {
			parentType = result.Task.EntityType
		}
		if parentID == 0 {
			parentID = result.Task.EntityID
		}
	}
	if _, ok := serviceapi.CatalogEntityType(parentType); !ok || parentID <= 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT e.event_id,e.created_by,e.event_type,e.entity_id,e.entity_type,e.value_before,e.value_after FROM crm_events e JOIN event_enrichment_links l ON l.installation_id=e.installation_id AND l.event_id=e.event_id WHERE l.installation_id=$1 AND l.object_kind=$2 AND l.object_key=$3 AND e.event_type='task_result_added'`, installation, serviceapi.ObjectTask, result.Key)
	if err != nil {
		return err
	}
	var events []serviceapi.Event
	for rows.Next() {
		var event serviceapi.Event
		if err = rows.Scan(&event.ID, &event.CreatedBy, &event.Type, &event.EntityID, &event.EntityType, &event.ValueBefore, &event.ValueAfter); err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	batch := &pgx.Batch{}
	queued := false
	for _, event := range events {
		object, ok := planNoteAfterTask(event, parentType, parentID)
		if !ok {
			continue
		}
		queued = true
		batch.Queue(`INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,parent_type,parent_id,object_id,state,reason_code,source,payload,run_after,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),now()) ON CONFLICT DO NOTHING`, installation, object.Kind, object.Key, object.ParentType, object.ParentID, object.ObjectID, object.State, object.Reason, object.Source, jsonbOrNull(object.Payload))
		batch.Queue(`INSERT INTO event_enrichment_links(installation_id,event_id,object_kind,object_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, installation, event.ID, object.Kind, object.Key)
	}
	if !queued {
		return nil
	}
	return tx.SendBatch(ctx, batch).Close()
}

func (s *Postgres) FailEnrichment(ctx context.Context, c EnrichmentClaim, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, o := range c.Objects {
		attempt := min(o.Attempts+1, s.cfg.MaxAttempts)
		state, reason, delay := enrichmentFailure(cause, attempt, s.cfg.MaxAttempts)
		_, err = tx.Exec(ctx, `UPDATE event_enrichment_objects SET state=$4,reason_code=$5,attempts=$6,lease_until=NULL,run_after=now()+$7*interval '1 millisecond',updated_at=now() WHERE installation_id=$1 AND object_kind=$2 AND object_key=$3 AND lease_token=$8 AND lease_until>now()`, c.InstallationID, c.Kind, o.Key, state, reason, attempt, delay.Milliseconds(), o.Token)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Postgres) deleteOrphanEnrichment(ctx context.Context, tx pgx.Tx, installation uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM event_enrichment_objects o WHERE o.installation_id=$1 AND NOT EXISTS(SELECT 1 FROM event_enrichment_links l WHERE l.installation_id=o.installation_id AND l.object_kind=o.object_kind AND l.object_key=o.object_key) AND o.ctid IN (SELECT o2.ctid FROM event_enrichment_objects o2 WHERE o2.installation_id=$1 AND NOT EXISTS(SELECT 1 FROM event_enrichment_links l WHERE l.installation_id=o2.installation_id AND l.object_kind=o2.object_kind AND l.object_key=o2.object_key) LIMIT $2)`, installation, s.cfg.RetentionBatch)
	return err
}

func jsonbOrNull(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}
