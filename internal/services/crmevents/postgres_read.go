package crmevents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type cursor struct {
	At int64  `json:"at"`
	ID string `json:"id"`
}

func (s *Postgres) Query(ctx context.Context, q serviceapi.Query, p serviceapi.Principal) (serviceapi.QueryResult, error) {
	var err error
	var after cursor
	if q.Cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || json.Unmarshal(b, &after) != nil || after.At < q.From || after.At > q.To || after.ID == "" {
			return serviceapi.QueryResult{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid event cursor")
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	defer tx.Rollback(ctx)
	result := serviceapi.QueryResult{Events: []serviceapi.Event{}, Summaries: []serviceapi.UserSummary{}}
	result.Status, err = s.status(ctx, tx, p)
	if err != nil {
		return result, err
	}
	if result.Status.State == "not_enabled" {
		return result, nil
	}
	rows, err := tx.Query(ctx, `SELECT event_id,extract(epoch from created_at)::bigint,created_by,event_type,entity_id,entity_type,value_before,value_after FROM crm_events WHERE installation_id=$1 AND created_at>=to_timestamp($2) AND created_at<=to_timestamp($3) AND (coalesce(cardinality($4::bigint[]),0)=0 OR created_by=ANY($4)) AND ($5::bigint=0 OR (created_at,event_id)>(to_timestamp($5),$6)) ORDER BY created_at,event_id LIMIT $7`, p.InstallationID, q.From, q.To, q.UserIDs, after.At, after.ID, q.Limit+1)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var event serviceapi.Event
		if err = rows.Scan(&event.ID, &event.CreatedAt, &event.CreatedBy, &event.Type, &event.EntityID, &event.EntityType, &event.ValueBefore, &event.ValueAfter); err != nil {
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
		last := result.Events[len(result.Events)-1]
		b, _ := json.Marshal(cursor{At: last.CreatedAt, ID: last.ID})
		result.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	rows, err = tx.Query(ctx, `SELECT created_by,count(*),extract(epoch from max(created_at))::bigint FROM crm_events WHERE installation_id=$1 AND created_at>=to_timestamp($2) AND created_at<=to_timestamp($3) AND (coalesce(cardinality($4::bigint[]),0)=0 OR created_by=ANY($4)) GROUP BY created_by ORDER BY created_by LIMIT 101`, p.InstallationID, q.From, q.To, q.UserIDs)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v serviceapi.UserSummary
		if err = rows.Scan(&v.UserID, &v.UniqueEvents, &v.LastEventAt); err != nil {
			return result, err
		}
		result.Summaries = append(result.Summaries, v)
	}
	if len(result.Summaries) > 100 {
		return result, serviceapi.Fail(serviceapi.ResourceExhausted, "specify at most 100 employees")
	}
	return result, rows.Err()
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
