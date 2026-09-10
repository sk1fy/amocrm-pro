package crmevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (s *Postgres) Schedule(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT s.installation_id,s.initial_hours,s.continuous_to FROM event_sources s WHERE next_poll_at<=now() AND state NOT IN ('reauth_required','disabled','paused','failed') AND EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=s.installation_id AND c.enabled) AND NOT EXISTS(SELECT 1 FROM event_jobs j WHERE j.installation_id=s.installation_id AND j.kind='current' AND j.status IN ('queued','running','retry','paused')) ORDER BY next_poll_at FOR UPDATE OF s SKIP LOCKED LIMIT 50`)
	if err != nil {
		return err
	}
	type source struct {
		id      uuid.UUID
		hours   int
		through *time.Time
	}
	var sources []source
	for rows.Next() {
		var x source
		if err = rows.Scan(&x.id, &x.hours, &x.through); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	to := s.cfg.Now().UTC().Truncate(time.Second)
	for _, x := range sources {
		from := to.Add(-time.Duration(x.hours) * time.Hour)
		if x.through != nil {
			from = x.through.Add(-s.cfg.Overlap)
		}
		if from.After(to) {
			continue
		}
		_, err = tx.Exec(ctx, `INSERT INTO event_jobs(id,installation_id,kind,priority,window_from,window_to,target_to) VALUES($1,$2,'current',10,$3,$4,$5) ON CONFLICT DO NOTHING`, uuid.New(), x.id, from, minTime(from.Add(s.cfg.Window), to), to)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE event_sources SET next_poll_at=$2,updated_at=now() WHERE installation_id=$1`, x.id, to.Add(s.cfg.PollInterval))
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Postgres) Claim(ctx context.Context) (Slice, error) {
	var c Slice
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	defer tx.Rollback(ctx)
	// A short owner-only admission lock enforces one active backfill page across
	// worker replicas. Current collection retains the remaining worker capacity.
	var admission bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(39081476392)`).Scan(&admission); err != nil {
		return c, err
	}
	if !admission {
		return c, ErrNoWork
	}
	var backgroundActive bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_jobs j JOIN event_sources s USING(installation_id) WHERE j.kind='backfill' AND j.status='running' AND s.lease_until>now())`).Scan(&backgroundActive); err != nil {
		return c, err
	}
	// The source lock serializes all kinds, including expired leases, across processes.
	err = tx.QueryRow(ctx, `SELECT s.installation_id,s.integration_id FROM event_sources s WHERE s.state NOT IN ('reauth_required','disabled','paused','failed') AND (s.lease_until IS NULL OR s.lease_until<now()) AND EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=s.installation_id AND c.enabled) AND EXISTS(SELECT 1 FROM event_jobs j WHERE j.installation_id=s.installation_id AND j.status IN ('queued','retry','running') AND j.run_after<=now() AND (NOT $1 OR j.kind='current')) ORDER BY (SELECT min(j.priority) FROM event_jobs j WHERE j.installation_id=s.installation_id AND j.status IN ('queued','retry','running') AND j.run_after<=now()),s.updated_at FOR UPDATE OF s SKIP LOCKED LIMIT 1`, backgroundActive).Scan(&c.InstallationID, &c.IntegrationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	err = tx.QueryRow(ctx, `SELECT id,kind,window_from,window_to,target_to,page,scan_pass,pass_digest,previous_digest,attempts FROM event_jobs WHERE installation_id=$1 AND status IN ('queued','retry','running') AND run_after<=now() AND (NOT $2 OR kind='current') ORDER BY priority,created_at FOR UPDATE LIMIT 1`, c.InstallationID, backgroundActive).Scan(&c.ID, &c.Kind, &c.From, &c.To, &c.Target, &c.Page, &c.Pass, &c.Digest, &c.Previous, &c.Attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	err = tx.QueryRow(ctx, `UPDATE event_sources SET lease_token=lease_token+1,lease_until=now()+$2*interval '1 millisecond',state='running',updated_at=now() WHERE installation_id=$1 RETURNING lease_token`, c.InstallationID, s.cfg.Lease.Milliseconds()).Scan(&c.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	_, err = tx.Exec(ctx, `UPDATE event_jobs SET status='running',lease_token=$2,updated_at=now() WHERE id=$1`, c.ID, c.Token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	_, err = tx.Exec(ctx, `UPDATE event_operations SET status='running',updated_at=now() WHERE id IN (SELECT operation_id FROM event_operation_jobs WHERE job_id=$1)`, c.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c, ErrNoWork
		}
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s *Postgres) lockClaim(ctx context.Context, tx pgx.Tx, c Slice) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT coalesce(lease_token=$2 AND lease_until>now(),false) FROM event_sources WHERE installation_id=$1 FOR UPDATE`, c.InstallationID, c.Token).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	var token int64
	var status string
	err = tx.QueryRow(ctx, `SELECT lease_token,status FROM event_jobs WHERE id=$1 FOR UPDATE`, c.ID).Scan(&token, &status)
	if err != nil {
		return err
	}
	if token != c.Token || status != "running" {
		return ErrLeaseLost
	}
	return nil
}
func (s *Postgres) Fail(ctx context.Context, c Slice, cause error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.lockClaim(ctx, tx, c); err != nil {
		return err
	}
	code := string(serviceapi.ErrorCode(cause))
	attempt := c.Attempts + 1
	status := "retry"
	sourceState := "retry"
	delay := time.Duration(1<<min(attempt, 8)) * time.Second
	var apiErr *serviceapi.Error
	if errors.As(cause, &apiErr) && apiErr.RetryAfter > delay {
		delay = min(apiErr.RetryAfter, 10*time.Minute)
	}
	if code == string(serviceapi.ReauthRequired) {
		status = "paused"
		sourceState = "reauth_required"
	} else if code == string(serviceapi.PermissionDenied) || code == string(serviceapi.Unauthenticated) {
		status = "paused"
		sourceState = "paused"
	} else if attempt >= s.cfg.MaxAttempts || code == string(serviceapi.InvalidArgument) {
		status = "paused"
		sourceState = "failed"
	}
	if c.Kind == "backfill" && sourceState == "failed" {
		sourceState = "idle"
	}
	_, err = tx.Exec(ctx, `UPDATE event_jobs SET status=$2,attempts=$3,run_after=now()+$4*interval '1 millisecond',error_code=$5,updated_at=now() WHERE id=$1`, c.ID, status, attempt, delay.Milliseconds(), code)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE event_sources SET state=$2,error_code=$3,lease_until=NULL,updated_at=now() WHERE installation_id=$1`, c.InstallationID, sourceState, code)
	if err != nil {
		return err
	}
	operationState := status
	if status == "paused" && sourceState != "reauth_required" && sourceState != "paused" {
		operationState = "failed"
	}
	_, err = tx.Exec(ctx, `UPDATE event_operations SET status=$2,error_code=$3,updated_at=now() WHERE id IN(SELECT operation_id FROM event_operation_jobs WHERE job_id=$1)`, c.ID, operationState, code)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Postgres) SavePage(ctx context.Context, c Slice, page serviceapi.EventPage) error {
	digest, err := pageDigest(c.Digest, page.Events)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.lockClaim(ctx, tx, c); err != nil {
		return err
	}
	processed := int64(len(page.Events))
	inserted, updated, dedup, err := s.writeEvents(ctx, tx, c, page.Events)
	if err != nil {
		return err
	}
	if err = s.enqueueEnrichment(ctx, tx, c.InstallationID, page.Events); err != nil {
		return err
	}
	status := "queued"
	errorCode := ""
	complete := false
	if page.HasNext {
		c.Page++
		c.Digest = digest
		if c.Page > s.cfg.MaxPages {
			status = "paused"
			errorCode = "page_limit_exceeded"
		}
	} else if c.Pass > 1 && digest == c.Previous {
		complete = true
	} else if c.Pass >= s.cfg.MaxPasses {
		status = "paused"
		errorCode = "pagination_unstable"
	} else {
		c.Pass++
		c.Page = 1
		c.Previous = digest
		c.Digest = ""
	}
	if complete {
		if err = s.cover(ctx, tx, c); err != nil {
			return err
		}
		if c.To.Before(c.Target) {
			c.From = c.To.Add(-s.cfg.Overlap)
			c.To = minTime(c.To.Add(s.cfg.Window), c.Target)
			c.Page = 1
			c.Pass = 1
			c.Digest = ""
			c.Previous = ""
		} else {
			status = "completed"
		}
	}
	_, err = tx.Exec(ctx, `UPDATE event_jobs SET status=$2,window_from=$3,window_to=$4,page=$5,scan_pass=$6,pass_digest=$7,previous_digest=$8,attempts=0,error_code=$9,run_after=now(),updated_at=now() WHERE id=$1`, c.ID, status, c.From, c.To, c.Page, c.Pass, c.Digest, c.Previous, errorCode)
	if err != nil {
		return err
	}
	sourceState := status
	if status == "queued" {
		sourceState = "pending"
	}
	if status == "completed" {
		sourceState = "idle"
	}
	if errorCode != "" {
		sourceState = "failed"
		if c.Kind == "backfill" {
			sourceState = "idle"
		}
	}
	var last *time.Time
	for _, e := range page.Events {
		t := time.Unix(e.CreatedAt, 0).UTC()
		if last == nil || t.After(*last) {
			last = &t
		}
	}
	_, err = tx.Exec(ctx, `UPDATE event_sources SET lease_until=NULL,state=$2,error_code=$3,last_event_at=greatest(last_event_at,$4::timestamptz),next_poll_at=CASE WHEN $5 THEN now()+$6*interval '1 millisecond' ELSE next_poll_at END,updated_at=now() WHERE installation_id=$1`, c.InstallationID, sourceState, errorCode, last, status == "completed", s.cfg.PollInterval.Milliseconds())
	if err != nil {
		return err
	}
	operationState := status
	if status == "queued" {
		operationState = "running"
	}
	if status == "paused" {
		operationState = "failed"
	}
	_, err = tx.Exec(ctx, `UPDATE event_jobs SET processed=processed+$2,inserted=inserted+$3,updated=updated+$4,deduplicated=deduplicated+$5 WHERE id=$1`, c.ID, processed, inserted, updated, dedup)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE event_sources SET events_processed=events_processed+$2,events_inserted=events_inserted+$3,events_updated=events_updated+$4,events_deduplicated=events_deduplicated+$5 WHERE installation_id=$1`, c.InstallationID, processed, inserted, updated, dedup)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE event_operations SET status=$2,error_code=$3,processed=processed+$4,inserted=inserted+$5,updated=updated+$6,deduplicated=deduplicated+$7,updated_at=now() WHERE id IN(SELECT operation_id FROM event_operation_jobs WHERE job_id=$1)`, c.ID, operationState, errorCode, processed, inserted, updated, dedup)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Postgres) cover(ctx context.Context, tx pgx.Tx, c Slice) error {
	// The source lease serializes these writes. Maintaining disjoint merged rows
	// makes one aggregate sufficient and avoids loading arbitrary history into a worker.
	var mergedFrom, mergedTo *time.Time
	err := tx.QueryRow(ctx, `SELECT min(window_from),max(window_to) FROM event_coverage WHERE installation_id=$1 AND window_from<=$3 AND window_to>=$2`, c.InstallationID, c.From, c.To).Scan(&mergedFrom, &mergedTo)
	if err != nil {
		return err
	}
	from, to := c.From, c.To
	if mergedFrom != nil && mergedFrom.Before(from) {
		from = *mergedFrom
	}
	if mergedTo != nil && mergedTo.After(to) {
		to = *mergedTo
	}
	_, err = tx.Exec(ctx, `DELETE FROM event_coverage WHERE installation_id=$1 AND window_from<=$3 AND window_to>=$2`, c.InstallationID, from, to)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_coverage(installation_id,window_from,window_to) VALUES($1,$2,$3)`, c.InstallationID, from, to)
	if err != nil {
		return err
	}
	var start, end *time.Time
	err = tx.QueryRow(ctx, `SELECT continuous_from,continuous_to FROM event_sources WHERE installation_id=$1`, c.InstallationID).Scan(&start, &end)
	if err != nil {
		return err
	}
	if start == nil && c.Kind == "current" {
		start = &from
		end = &to
	}
	if start != nil && !from.After(*end) && !to.Before(*start) {
		if from.Before(*start) {
			*start = from
		}
		if to.After(*end) {
			*end = to
		}
	}
	_, err = tx.Exec(ctx, `UPDATE event_sources SET continuous_from=$2,continuous_to=$3,last_success_at=now(),updated_at=now() WHERE installation_id=$1`, c.InstallationID, start, end)
	return err
}

// Retain spends one bounded batch on the least recently visited source. It
// shares the owner source lock with page persistence and never locks all tenants.
// retained_from is a conservative guaranteed-history frontier, not a claim that
// every older row has physically disappeared. Partial batches advance only past
// their deleted prefix, including a whole timestamp tie; no event-presence query
// is used to infer that an interval was verified. Product retention_days stay
// 2..30 and are never clamped here. One tick also runs one bounded technical-
// history batch; ingest faster than cleanup may lag and is not drained in a loop.
func (s *Postgres) Retain(ctx context.Context) (int64, error) {
	removed, err := s.retainEvents(ctx)
	if err != nil {
		return removed, err
	}
	if _, err = s.retainTechnicalHistory(ctx); err != nil {
		return removed, err
	}
	return removed, nil
}

func (s *Postgres) retainEvents(ctx context.Context) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var installation uuid.UUID
	var cutoff time.Time
	err = tx.QueryRow(ctx, `SELECT installation_id,date_trunc('second',now()-retention_days*interval '1 day') FROM event_sources ORDER BY retention_checked_at NULLS FIRST,installation_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&installation, &cutoff)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var removed int64
	var latest *time.Time
	err = tx.QueryRow(ctx, `WITH doomed AS (SELECT event_id FROM crm_events WHERE installation_id=$1 AND created_at<$2 ORDER BY created_at,event_id LIMIT $3), deleted AS (DELETE FROM crm_events e USING doomed d WHERE e.installation_id=$1 AND e.event_id=d.event_id RETURNING e.created_at) SELECT count(*),max(created_at) FROM deleted`, installation, cutoff, s.cfg.RetentionBatch).Scan(&removed, &latest)
	if err != nil {
		return 0, err
	}
	frontier := cutoff
	if removed == int64(s.cfg.RetentionBatch) && latest != nil {
		frontier = minTime(latest.Truncate(time.Second).Add(time.Second), cutoff)
	}
	_, err = tx.Exec(ctx, `UPDATE event_sources SET retained_from=greatest(retained_from,$2::timestamptz),retention_checked_at=clock_timestamp() WHERE installation_id=$1`, installation, frontier)
	if err != nil {
		return 0, err
	}
	if err = s.deleteOrphanEnrichment(ctx, tx, installation); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return removed, nil
}

const technicalHistoryLockID int64 = 39081476393

// retainTechnicalHistory deletes one bounded batch of owner technical history.
// Horizon is independent of retention_days and is never shorter than
// TechnicalHistoryHorizon. Paused work remains resumable. Terminal inbox and
// operations older than the same calendar horizon as Core redelivery are
// collected; a tombstone rejects late Apply of that command_id.
func (s *Postgres) retainTechnicalHistory(ctx context.Context) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, technicalHistoryLockID).Scan(&locked); err != nil {
		return 0, err
	}
	if !locked {
		return 0, tx.Commit(ctx)
	}
	horizon, batch := s.cfg.HistoryHorizon.Milliseconds(), int64(s.cfg.HistoryBatch)
	var removed int64
	var n int64
	if err = tx.QueryRow(ctx, `WITH doomed AS (
		SELECT j.id FROM event_jobs j
		WHERE j.status IN ('completed','failed')
		  AND j.updated_at < now()-($1*interval '1 millisecond')
		  AND NOT EXISTS (
			SELECT 1 FROM event_sources s
			WHERE s.installation_id=j.installation_id AND s.lease_until>now() AND s.lease_token=j.lease_token
		  )
		ORDER BY j.updated_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT $2
	), unlinked AS (DELETE FROM event_operation_jobs oj USING doomed d WHERE oj.job_id=d.id RETURNING oj.job_id),
	deleted AS (DELETE FROM event_jobs j USING doomed d WHERE j.id=d.id RETURNING j.id)
	SELECT (SELECT count(*) FROM deleted)+0*(SELECT count(*) FROM unlinked)`, horizon, batch).Scan(&n); err != nil {
		return 0, err
	}
	removed += n
	// Catalog objects stay while linked; this pass only bounds orphans that
	// outlived deleted events and were not drained by the per-source Retain turn.
	if err = tx.QueryRow(ctx, `WITH doomed AS (
		SELECT o.ctid FROM event_enrichment_objects o
		WHERE NOT EXISTS (
			SELECT 1 FROM event_enrichment_links l
			WHERE l.installation_id=o.installation_id AND l.object_kind=o.object_kind AND l.object_key=o.object_key
		) AND (o.lease_until IS NULL OR o.lease_until<now())
		ORDER BY o.updated_at,o.ctid FOR UPDATE OF o SKIP LOCKED LIMIT $1
	), deleted AS (DELETE FROM event_enrichment_objects o USING doomed d WHERE o.ctid=d.ctid RETURNING o.ctid)
	SELECT count(*) FROM deleted`, batch).Scan(&n); err != nil {
		return 0, err
	}
	removed += n
	if err = tx.QueryRow(ctx, `WITH doomed AS (
		SELECT c.ctid FROM event_coverage c
		JOIN event_sources s ON s.installation_id=c.installation_id
		WHERE s.retained_from IS NOT NULL AND c.window_to<=s.retained_from
		ORDER BY c.window_to,c.ctid FOR UPDATE OF c SKIP LOCKED LIMIT $1
	), deleted AS (DELETE FROM event_coverage c USING doomed d WHERE c.ctid=d.ctid RETURNING c.ctid)
	SELECT count(*) FROM deleted`, batch).Scan(&n); err != nil {
		return 0, err
	}
	removed += n
	if err = tx.QueryRow(ctx, `WITH doomed AS (
		SELECT i.installation_id,i.command_id,i.payload_hash,i.created_at,i.operation_id
		FROM event_inbox i
		JOIN event_operations o ON o.id=i.operation_id
		WHERE o.status IN ('completed','failed')
		  AND i.created_at < now()-($1*interval '1 millisecond')
		  AND o.updated_at < now()-($1*interval '1 millisecond')
		  AND NOT EXISTS (SELECT 1 FROM event_jobs j WHERE j.operation_id=o.id)
		  AND NOT EXISTS (SELECT 1 FROM event_operation_jobs oj WHERE oj.operation_id=o.id)
		ORDER BY i.created_at,i.command_id FOR UPDATE OF i,o SKIP LOCKED LIMIT $2
	), marked AS (
		INSERT INTO event_command_tombstones(installation_id,command_id,payload_hash,command_created_at)
		SELECT installation_id,command_id,payload_hash,created_at FROM doomed
		ON CONFLICT DO NOTHING RETURNING command_id
	), deleted_inbox AS (
		DELETE FROM event_inbox i USING doomed d
		WHERE i.installation_id=d.installation_id AND i.command_id=d.command_id
		RETURNING i.command_id
	), deleted_ops AS (
		DELETE FROM event_operations o USING doomed d WHERE o.id=d.operation_id
		RETURNING o.id
	)
	SELECT (SELECT count(*) FROM deleted_inbox)+0*(SELECT count(*) FROM marked)+0*(SELECT count(*) FROM deleted_ops)`, horizon, batch).Scan(&n); err != nil {
		return 0, err
	}
	removed += n
	if err = tx.QueryRow(ctx, `WITH doomed AS (
		SELECT t.ctid FROM event_command_tombstones t
		WHERE t.tombstoned_at < now()-($1*interval '1 millisecond')
		ORDER BY t.tombstoned_at,t.ctid FOR UPDATE OF t SKIP LOCKED LIMIT $2
	), deleted AS (DELETE FROM event_command_tombstones t USING doomed d WHERE t.ctid=d.ctid RETURNING t.ctid)
	SELECT count(*) FROM deleted`, horizon, batch).Scan(&n); err != nil {
		return 0, err
	}
	removed += n
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return removed, nil
}

// One owner-locked hash read plus one pgx batch replaces two wire round trips
// per event. Duplicate IDs are folded after sequential counter accounting, so a
// page containing insert/change/replay of one ID retains the original semantics.
func (s *Postgres) writeEvents(ctx context.Context, tx pgx.Tx, c Slice, events []serviceapi.Event) (inserted, updated, dedup int64, err error) {
	if len(events) == 0 {
		return
	}
	ordered := append([]serviceapi.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	ids := make([]string, 0, len(ordered))
	for _, e := range ordered {
		ids = append(ids, e.ID)
	}
	hashes := make(map[string][]byte, len(ordered))
	rows, err := tx.Query(ctx, `SELECT event_id,content_hash FROM crm_events WHERE installation_id=$1 AND event_id=ANY($2::text[])`, c.InstallationID, ids)
	if err != nil {
		return
	}
	for rows.Next() {
		var id string
		var hash []byte
		if err = rows.Scan(&id, &hash); err != nil {
			rows.Close()
			return
		}
		hashes[id] = hash
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	batch := &pgx.Batch{}
	for i, raw := range ordered {
		var event serviceapi.Event
		var b []byte
		event, b, err = canonicalEvent(raw)
		if err != nil {
			return
		}
		hash := sha256.Sum256(b)
		old, exists := hashes[event.ID]
		if !exists {
			inserted++
		} else if bytes.Equal(old, hash[:]) {
			dedup++
		} else {
			updated++
		}
		hashes[event.ID] = hash[:]
		if i+1 < len(ordered) && ordered[i+1].ID == event.ID {
			continue
		}
		batch.Queue(`INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,value_before,value_after,content_hash,linked_talk_contact_id) VALUES($1,$2,to_timestamp($3),$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(installation_id,event_id) DO UPDATE SET created_at=excluded.created_at,created_by=excluded.created_by,event_type=excluded.event_type,entity_id=excluded.entity_id,entity_type=excluded.entity_type,value_before=excluded.value_before,value_after=excluded.value_after,content_hash=excluded.content_hash,linked_talk_contact_id=excluded.linked_talk_contact_id,observed_at=now()`, c.InstallationID, event.ID, event.CreatedAt, event.CreatedBy, event.Type, event.EntityID, event.EntityType, event.ValueBefore, event.ValueAfter, hash[:], event.LinkedTalkContactID)
	}
	err = tx.SendBatch(ctx, batch).Close()
	return
}
