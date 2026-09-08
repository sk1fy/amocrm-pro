package crmevents

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

const eventColumns = `e.event_id,extract(epoch from e.created_at)::bigint,e.created_by,e.event_type,e.entity_id,e.entity_type,e.linked_talk_contact_id`

// Both the page and its whole-period summaries use this predicate. Exact types
// and a literal prefix are alternatives; every other filter narrows the result.
var eventHistoryFrom = ` FROM crm_events e JOIN event_sources s ON s.installation_id=e.installation_id WHERE e.installation_id=$1 AND s.integration_id=$2 AND e.created_at>=to_timestamp($3) AND e.created_at<=to_timestamp($4) AND ((coalesce(cardinality($5::bigint[]),0)=0 OR e.created_by=ANY($5)) OR ($11::bool AND (e.created_by=0 OR (coalesce(cardinality($12::bigint[]),0)>0 AND NOT e.created_by=ANY($12))))) AND ((coalesce(cardinality($6::text[]),0)=0 AND $7::text='') OR e.event_type=ANY($6) OR ($7::text<>'' AND left(e.event_type,char_length($7))=$7)) AND ($8::text='' OR e.entity_type=$8) AND (coalesce(cardinality($9::bigint[]),0)=0 OR e.entity_id=ANY($9)) AND (coalesce(cardinality($10::text[]),0)=0 OR (` + eventCategorySQL("e.event_type") + `)=ANY($10))`

func historyArgs(q serviceapi.Query, p serviceapi.Principal) []any {
	return []any{p.InstallationID, p.IntegrationID, q.From, q.To, q.UserIDs, q.Types, q.TypePrefix, q.EntityType, q.EntityIDs, q.Categories, q.IncludeUnknownAuthors, q.DirectoryUserIDs}
}

func (s *Postgres) Query(ctx context.Context, q serviceapi.Query, p serviceapi.Principal) (serviceapi.QueryResult, error) {
	after, err := decodeCursor(q, p)
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	buckets, err := serviceapi.QueryTimeBuckets(q)
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	defer tx.Rollback(ctx)
	result := serviceapi.QueryResult{Events: []serviceapi.Event{}, Summaries: []serviceapi.UserSummary{}, ReadVersion: serviceapi.PresentationReadVersion, PayloadsOmitted: q.Compact}
	result.Status, err = s.status(ctx, tx, p)
	if err != nil {
		return result, err
	}
	if result.Status.State == "not_enabled" {
		return result, nil
	}
	args := historyArgs(q, p)
	projection := eventColumns + `,e.value_before,e.value_after`
	if q.Compact {
		// Do not read or marshal the large JSON columns for a compact journal.
		projection = eventColumns + `,NULL::jsonb,NULL::jsonb`
	}
	comparison, order := ">", "ASC"
	if q.Order == "desc" {
		comparison, order = "<", "DESC"
	}
	pageSQL := `SELECT ` + projection + eventHistoryFrom + ` AND ($13::bool=false OR (e.created_at,e.event_id)` + comparison + `(to_timestamp($14),$15)) ORDER BY e.created_at ` + order + `,e.event_id ` + order + ` LIMIT $16`
	pageArgs := append(append([]any{}, args...), q.Cursor != "", after.At, after.ID, q.Limit+1)
	rows, err := tx.Query(ctx, pageSQL, pageArgs...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var event serviceapi.Event
		if err = scanEvent(rows, &event); err != nil {
			rows.Close()
			return result, err
		}
		result.Events = append(result.Events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(result.Events) > q.Limit {
		result.Events = result.Events[:q.Limit]
		result.NextCursor = encodeCursor(q, p, result.Events[len(result.Events)-1])
	}
	rows, err = tx.Query(ctx, `SELECT e.created_by,count(*),coalesce(extract(epoch from min(e.created_at))::bigint,0),extract(epoch from max(e.created_at))::bigint,count(distinct (e.entity_type || ':' || e.entity_id::text)),count(*) FILTER (WHERE e.event_type='task_completed'),count(distinct e.entity_id) FILTER (WHERE e.event_type='task_completed')`+eventHistoryFrom+` GROUP BY e.created_by ORDER BY e.created_by LIMIT 101`, args...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var v serviceapi.UserSummary
		if err = rows.Scan(&v.UserID, &v.UniqueEvents, &v.FirstEventAt, &v.LastEventAt, &v.EntityCount, &v.TaskCompletedEvents, &v.UniqueCompletedTasks); err != nil {
			rows.Close()
			return result, err
		}
		result.Summaries = append(result.Summaries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(result.Summaries) > 100 {
		return result, serviceapi.Fail(serviceapi.ResourceExhausted, "specify at most 100 employees")
	}
	if err = scanCategoryCounts(ctx, tx, args, &result); err != nil {
		return result, err
	}
	if err = scanTotals(ctx, tx, args, &result); err != nil {
		return result, err
	}
	if len(buckets) > 0 {
		if err = scanTimeline(ctx, tx, q, buckets, args, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func scanCategoryCounts(ctx context.Context, tx pgx.Tx, args []any, result *serviceapi.QueryResult) error {
	rows, err := tx.Query(ctx, `SELECT e.created_by,`+eventCategorySQL("e.event_type")+`,count(*)`+eventHistoryFrom+` GROUP BY 1,2 ORDER BY 1,2`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byUser := map[int64][]serviceapi.CategoryCount{}
	var totals []serviceapi.CategoryCount
	index := map[string]int{}
	for rows.Next() {
		var userID int64
		var item serviceapi.CategoryCount
		if err = rows.Scan(&userID, &item.Category, &item.Count); err != nil {
			return err
		}
		byUser[userID] = append(byUser[userID], item)
		if i, ok := index[item.Category]; ok {
			totals[i].Count += item.Count
		} else {
			index[item.Category] = len(totals)
			totals = append(totals, item)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i, summary := range result.Summaries {
		result.Summaries[i].CategoryCounts = byUser[summary.UserID]
	}
	result.Totals.CategoryCounts = totals
	return nil
}

func scanTotals(ctx context.Context, tx pgx.Tx, args []any, result *serviceapi.QueryResult) error {
	row := tx.QueryRow(ctx, `SELECT count(*),count(distinct (e.entity_type || ':' || e.entity_id::text)),count(*) FILTER (WHERE e.event_type='task_completed'),count(distinct e.entity_id) FILTER (WHERE e.event_type='task_completed'),coalesce(extract(epoch from min(e.created_at))::bigint,0),coalesce(extract(epoch from max(e.created_at))::bigint,0)`+eventHistoryFrom, args...)
	err := row.Scan(&result.Totals.UniqueEvents, &result.Totals.EntityCount, &result.Totals.TaskCompletedEvents, &result.Totals.UniqueCompletedTasks, &result.Totals.FirstEventAt, &result.Totals.LastEventAt)
	return err
}

func scanTimeline(ctx context.Context, tx pgx.Tx, q serviceapi.Query, buckets []serviceapi.TimeBucket, args []any, result *serviceapi.QueryResult) error {
	starts := make([]int64, len(buckets))
	for i, bucket := range buckets {
		starts[i] = bucket.StartAt
	}
	// width_bucket assigns each event to the same bounded, absolute intervals
	// used by Go. The shared WHERE clips the first/last interval to the query.
	rows, err := tx.Query(ctx, `SELECT width_bucket(extract(epoch from e.created_at)::bigint,$13::bigint[]),count(*)`+eventHistoryFrom+` GROUP BY 1`, append(append([]any{}, args...), starts)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var count int64
		if err = rows.Scan(&index, &count); err != nil {
			return err
		}
		if index < 1 || index > len(buckets) {
			return serviceapi.Fail(serviceapi.Internal, "event outside timeline bounds")
		}
		buckets[index-1].Count = count
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range buckets {
		buckets[i].Coverage = bucketCoverage(max(q.From, buckets[i].StartAt), buckets[i].EndAt, result.Status)
	}
	result.Timeline = buckets
	return nil
}

func bucketCoverage(from, to int64, status serviceapi.SyncStatus) string {
	if status.VerifiedThrough == 0 || status.VerifiedFrom == 0 {
		return serviceapi.CoverageUnknown
	}
	if to < status.VerifiedFrom || to < status.HistoryFrom {
		return serviceapi.CoverageUnknown
	}
	if from < status.VerifiedFrom || from < status.HistoryFrom || to > status.VerifiedThrough {
		return serviceapi.CoveragePartial
	}
	return serviceapi.CoverageVerified
}

func scanEvent(row interface{ Scan(...any) error }, event *serviceapi.Event) error {
	return row.Scan(&event.ID, &event.CreatedAt, &event.CreatedBy, &event.Type, &event.EntityID, &event.EntityType, &event.LinkedTalkContactID, &event.ValueBefore, &event.ValueAfter)
}

func (s *Postgres) GetEvent(ctx context.Context, p serviceapi.Principal, id string) (serviceapi.Event, error) {
	var event serviceapi.Event
	row := s.pool.QueryRow(ctx, `SELECT `+eventColumns+`,e.value_before,e.value_after FROM crm_events e JOIN event_sources s ON s.installation_id=e.installation_id WHERE e.installation_id=$1 AND s.integration_id=$2 AND e.event_id=$3`, p.InstallationID, p.IntegrationID, id)
	if err := scanEvent(row, &event); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "event not found")
		}
		return serviceapi.Event{}, err
	}
	return event, nil
}

// LoadEventEnrichment attaches owner-local sidecar objects and catalog names.
// Query never joins these bodies; list rows stay historical envelopes.
func (s *Postgres) LoadEventEnrichment(ctx context.Context, p serviceapi.Principal, event serviceapi.Event) (serviceapi.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.object_kind,o.object_key,o.state,o.reason_code,o.source,coalesce(extract(epoch from o.fetched_at)::bigint,0),o.payload FROM event_enrichment_links l JOIN event_enrichment_objects o ON o.installation_id=l.installation_id AND o.object_kind=l.object_kind AND o.object_key=l.object_key WHERE l.installation_id=$1 AND l.event_id=$2 ORDER BY o.object_kind,o.object_key`, p.InstallationID, event.ID)
	if err != nil {
		return event, err
	}
	defer rows.Close()
	objects := eventPayloadObjects(event)
	historical := map[string]bool{}
	for _, o := range objects {
		historical[o.ObjectKind+"\x00"+o.ObjectKey] = true
	}
	for rows.Next() {
		var o serviceapi.EnrichmentObject
		var payload []byte
		if err = rows.Scan(&o.ObjectKind, &o.ObjectKey, &o.State, &o.ReasonCode, &o.Source, &o.FetchedAt, &payload); err != nil {
			return event, err
		}
		if len(payload) > 0 && string(payload) != "null" {
			o.Payload = payload
		}
		if o.Source == serviceapi.SourceEventPayload || historical[o.ObjectKind+"\x00"+o.ObjectKey] {
			continue
		}
		o.Current = enrichmentCurrent(o.State, o.Source, o.Payload)
		objects = append(objects, o)
	}
	if err = rows.Err(); err != nil {
		return event, err
	}
	return attachSidecar(event, objects), nil
}
func (s *Postgres) Status(ctx context.Context, p serviceapi.Principal) (serviceapi.SyncStatus, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	defer tx.Rollback(ctx)
	return s.status(ctx, tx, p)
}

func (s *Postgres) status(ctx context.Context, tx pgx.Tx, p serviceapi.Principal) (serviceapi.SyncStatus, error) {
	var state serviceapi.SyncStatus
	var from, through, retained, success, last *time.Time
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT bool_or(enabled) FROM event_consumers WHERE installation_id=s.installation_id),false),s.state,s.continuous_from,s.continuous_to,s.retained_from,s.last_success_at,s.last_event_at,s.error_code FROM event_sources s WHERE installation_id=$1 AND integration_id=$2`, p.InstallationID, p.IntegrationID).Scan(&state.Enabled, &state.State, &from, &through, &retained, &success, &last, &state.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		state.State = "not_enabled"
		return state, nil
	}
	if err != nil {
		return state, err
	}
	epoch := func(t *time.Time) int64 {
		if t == nil {
			return 0
		}
		return t.Unix()
	}
	state.VerifiedFrom = epoch(from)
	state.VerifiedThrough = epoch(through)
	state.HistoryFrom = epoch(retained)
	state.LastSuccessAt = epoch(success)
	state.LastEventAt = epoch(last)
	state.HistoryFrom = max(state.HistoryFrom, state.VerifiedFrom)
	if through != nil {
		state.LagSeconds = max(0, s.cfg.Now().Unix()-through.Unix())
	}
	state.ReauthRequired = state.ErrorCode == string(serviceapi.ReauthRequired)
	// These are authorization-visible, replay-stabilized API scans, not an amoCRM snapshot guarantee.
	state.Verification = "stabilized_api_scan"
	var wf, wt time.Time
	err = tx.QueryRow(ctx, `SELECT window_from,window_to,page FROM event_jobs WHERE installation_id=$1 AND status IN ('queued','running','retry','paused') ORDER BY priority,created_at LIMIT 1`, p.InstallationID).Scan(&wf, &wt, &state.NextPage)
	if err == nil {
		state.WindowFrom = wf.Unix()
		state.WindowTo = wt.Unix()
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return state, err
	}
	return state, nil
}
