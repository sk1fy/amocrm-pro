package adminread

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

type DistributionBinding struct {
	ID                uuid.UUID `json:"id"`
	CompanyID         uuid.UUID `json:"company_id"`
	State             string    `json:"state"`
	Revision          int64     `json:"revision"`
	MappingRevision   int64     `json:"mapping_revision"`
	MappedEmployees   int64     `json:"mapped_employees"`
	ServiceAuthorized bool      `json:"service_authorized"`
}
type DistributionBacklog struct {
	States          map[string]int64 `json:"states"`
	OldestPendingAt *time.Time       `json:"oldest_pending_at"`
}
type DistributionSummary struct {
	Source             string               `json:"source"`
	ObservedAt         time.Time            `json:"observed_at"`
	InstallationID     uuid.UUID            `json:"installation_id"`
	ModuleEnabled      bool                 `json:"module_enabled"`
	Paused             bool                 `json:"paused"`
	AuthorizationState string               `json:"authorization_state"`
	WebhookState       string               `json:"webhook_state"`
	WebhookCheckedAt   *time.Time           `json:"webhook_checked_at"`
	Binding            *DistributionBinding `json:"binding"`
	Events             DistributionBacklog  `json:"events"`
	Results            DistributionBacklog  `json:"results"`
	Operations         map[string]int64     `json:"operations"`
	HistoricalGaps     int64                `json:"historical_gaps"`
	TeamQueueState     string               `json:"team_queue_state"`
}

func (h *handler) getDistribution(w http.ResponseWriter, r *http.Request) {
	id, e := uuid.Parse(chi.URLParam(r, "id"))
	if e != nil || id == uuid.Nil {
		writeError(w, r, errInvalid("invalid installation identifier"))
		return
	}
	observed := time.Now().UTC()
	card, e := h.store.getInstallation(r.Context(), id, observed)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	ctx, cancel := h.store.withTimeout(r.Context())
	defer cancel()
	out := DistributionSummary{Source: sourceCore, ObservedAt: observed, InstallationID: id, AuthorizationState: card.Authorization.State, WebhookState: card.Webhook.Status, WebhookCheckedAt: card.Webhook.CheckedAt, TeamQueueState: "unknown", Operations: map[string]int64{}, Events: DistributionBacklog{States: map[string]int64{}}, Results: DistributionBacklog{States: map[string]int64{}}}
	out.ModuleEnabled, e = services.NewStore(h.store.pool).IsEnabled(ctx, card.Installation.IntegrationID, id, services.LeadDistribution)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	e = h.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_admin_pauses WHERE installation_id=$1 AND paused)`, id).Scan(&out.Paused)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	var b DistributionBinding
	e = h.store.pool.QueryRow(ctx, `SELECT b.id,b.company_id,b.state,b.revision,b.mapping_revision,(SELECT count(*) FROM distribution_actor_mappings m WHERE m.binding_id=b.id),b.state='active' AND EXISTS(SELECT 1 FROM distribution_service_grants g WHERE g.company_id=b.company_id AND g.installation_id=b.installation_id AND g.enabled) FROM distribution_bindings b WHERE b.installation_id=$1 ORDER BY (b.state='active') DESC,b.created_at DESC,b.id DESC LIMIT 1`, id).Scan(&b.ID, &b.CompanyID, &b.State, &b.Revision, &b.MappingRevision, &b.MappedEmployees, &b.ServiceAuthorized)
	if e == nil {
		b.ServiceAuthorized = b.ServiceAuthorized && out.ModuleEnabled
		out.Binding = &b
	} else if !errors.Is(e, pgx.ErrNoRows) {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	for kind, target := range map[string]*DistributionBacklog{"events": &out.Events, "results": &out.Results} {
		table, predicate := "distribution_event_outbox", "scope->>'installationId'=$1"
		if kind == "results" {
			table = "distribution_result_outbox"
			predicate = "operation_id IN (SELECT id FROM distribution_operations WHERE installation_id=$1::uuid)"
		}
		rows, err := h.store.pool.Query(ctx, `SELECT state,count(*),min(created_at) FILTER(WHERE state<>'acknowledged') FROM `+table+` WHERE `+predicate+` GROUP BY state`, id.String())
		if err != nil {
			h.queryFailed(w, r, distributionReadError(err))
			return
		}
		for rows.Next() {
			var state string
			var n int64
			var oldest *time.Time
			if err = rows.Scan(&state, &n, &oldest); err != nil {
				break
			}
			target.States[state] = n
			if oldest != nil && (target.OldestPendingAt == nil || oldest.Before(*target.OldestPendingAt)) {
				target.OldestPendingAt = oldest
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			h.queryFailed(w, r, distributionReadError(err))
			return
		}
	}
	rows, e := h.store.pool.Query(ctx, `SELECT state,count(*) FROM distribution_operations WHERE installation_id=$1 GROUP BY state`, id)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	for rows.Next() {
		var state string
		var n int64
		if e = rows.Scan(&state, &n); e != nil {
			break
		}
		out.Operations[state] = n
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	e = h.store.pool.QueryRow(ctx, `SELECT count(*) FROM distribution_recovery_scans WHERE binding_id IN(SELECT id FROM distribution_bindings WHERE installation_id=$1) AND gap_reason<>''`, id).Scan(&out.HistoricalGaps)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	writeJSON(w, 200, out)
}

type DistributionTraceItem struct {
	MessageID           *uuid.UUID `json:"message_id"`
	Kind                string     `json:"kind"`
	ID                  string     `json:"id"`
	State               string     `json:"state"`
	CreatedAt           time.Time  `json:"created_at"`
	ErrorCode           *string    `json:"error_code"`
	EventID             *uuid.UUID `json:"event_id"`
	OperationID         *uuid.UUID `json:"operation_id"`
	CorrelationID       *uuid.UUID `json:"correlation_id"`
	CausationID         *uuid.UUID `json:"causation_id"`
	LeadID              *string    `json:"lead_id"`
	ResultVersion       *int64     `json:"result_version"`
	ExternalEffectState *string    `json:"external_effect_state"`
	Evidence            *string    `json:"evidence"`
	Attempts            *int       `json:"attempts"`
}

// Only explicitly selected diagnostic fields cross the listener. Durable bodies
// and CRM snapshots remain private. Rows from other installations never join.
const distributionTraceSQL = `WITH seed_events AS (
 SELECT message_id::text message_id,payload->>'eventId' event_id,consumer_receipt_id FROM distribution_event_outbox
 WHERE scope->>'installationId'=$1::text AND (message_id::text=$2 OR payload->>'eventId'=$2 OR payload->>'correlationId'=$2 OR consumer_receipt_id::text=$2)
), related AS (
 SELECT o.id FROM distribution_operations o WHERE o.installation_id=$1::uuid AND ($2='' OR o.id::text=$2 OR o.decision_id::text=$2 OR o.command->>'messageId'=$2 OR o.command->>'eventId'=$2 OR o.command->>'correlationId'=$2 OR o.command->>'causationId'=$2 OR o.key_hash=$6::bytea
 OR o.command->>'causationId' IN(SELECT message_id FROM seed_events) OR o.command->>'causationId' IN(SELECT event_id FROM seed_events) OR o.command->>'eventId' IN(SELECT event_id FROM seed_events)
 OR o.id IN(SELECT operation_id FROM distribution_result_outbox WHERE payload->>'messageId'=$2))
), related_events AS (
 SELECT e.message_id,e.state,e.created_at,e.error_code,e.payload,e.attempts,e.consumer_receipt_id,e.recovery_scan_id FROM distribution_event_outbox e WHERE e.scope->>'installationId'=$1::text AND ($2='' OR e.message_id::text IN(SELECT message_id FROM seed_events)
 OR e.message_id::text IN(SELECT command->>'causationId' FROM distribution_operations WHERE id IN(SELECT id FROM related))
 OR e.payload->>'eventId' IN(SELECT command->>'causationId' FROM distribution_operations WHERE id IN(SELECT id FROM related))
 OR e.payload->>'eventId' IN(SELECT command->>'eventId' FROM distribution_operations WHERE id IN(SELECT id FROM related)))
), trace AS (
 SELECT 'event' kind,message_id::text id,state,created_at,error_code,payload->>'eventId' event_id,NULL::text operation_id,payload->>'correlationId' correlation_id,NULL::text causation_id,payload->'event'->>'leadId' lead_id,NULL::bigint result_version,NULL::text external_effect_state,NULL::text evidence,attempts FROM related_events
 UNION ALL SELECT 'operation',id::text,state,created_at,error_code,command->>'eventId',id::text,command->>'correlationId',command->>'causationId',lead_id::text,result_version,external_effect_state,evidence,NULL::integer FROM distribution_operations WHERE id IN(SELECT id FROM related)
 UNION ALL SELECT 'result',r.operation_id::text||':'||r.result_version::text,r.state,r.created_at,r.error_code,NULL::text,r.operation_id::text,NULL::text,NULL::text,NULL::text,r.result_version,NULL::text,NULL::text,r.attempts FROM distribution_result_outbox r JOIN distribution_operations o ON o.id=r.operation_id WHERE o.installation_id=$1::uuid AND ($2='' OR o.id IN(SELECT id FROM related))
 UNION ALL SELECT 'scan',s.id::text,s.state,s.created_at,s.error_code,NULL::text,NULL::text,s.id::text,NULL::text,NULL::text,NULL::bigint,NULL::text,s.gap_reason,s.attempts FROM distribution_recovery_scans s JOIN distribution_bindings b ON b.id=s.binding_id WHERE b.installation_id=$1::uuid AND ($2='' OR s.id::text=$2 OR s.id IN(SELECT recovery_scan_id FROM related_events))
 UNION ALL SELECT 'consumer',id::text,state,created_at,disposition,event_id::text,NULL::text,NULL::text,NULL::text,NULL::text,NULL::bigint,NULL::text,NULL::text,NULL::integer FROM webhook_consumer_receipts WHERE installation_id=$1::uuid AND consumer_id='core-lead-distribution-v1' AND ($2='' OR id::text=$2 OR event_id::text=$2 OR id IN(SELECT consumer_receipt_id FROM related_events))
) SELECT kind,id,state,created_at,error_code,event_id,operation_id,correlation_id,causation_id,lead_id,result_version,external_effect_state,evidence,attempts, CASE WHEN kind='event' THEN id WHEN kind='result' THEN (SELECT payload->>'messageId' FROM distribution_result_outbox WHERE operation_id::text||':'||result_version::text=trace.id) ELSE NULL END message_id FROM trace WHERE ($3::timestamptz IS NULL OR (created_at,kind||':'||id)<($3,$4)) ORDER BY created_at DESC,kind||':'||id DESC LIMIT $5`

func (h *handler) distributionTrace(w http.ResponseWriter, r *http.Request) {
	scope, e := h.installationScope(r)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	q := r.URL.Query()
	limit, e := parseLimit(q.Get("limit"))
	if e != nil {
		writeError(w, r, e)
		return
	}
	reference := q.Get("reference")
	if reference != "" {
		id, err := uuid.Parse(reference)
		if err != nil || id == uuid.Nil {
			writeError(w, r, errInvalid("reference must be a UUID"))
			return
		}
		reference = id.String()
	}
	at, key, e := decodeCursor(q.Get("cursor"))
	if e != nil {
		writeError(w, r, e)
		return
	}
	if len(key) > 100 {
		writeError(w, r, errInvalid("invalid cursor"))
		return
	}
	var after *time.Time
	if !at.IsZero() {
		after = &at
	}
	ctx, cancel := h.store.withTimeout(r.Context())
	defer cancel()
	items, e := h.store.distributionTrace(ctx, scope.InstallationID, reference, after, key, limit)
	if e != nil {
		h.queryFailed(w, r, distributionReadError(e))
		return
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		value := encodeCursor(last.CreatedAt, last.Kind+":"+last.ID)
		next = &value
	}
	writeJSON(w, 200, listResponse{Source: sourceCore, ObservedAt: time.Now().UTC(), Items: items, NextCursor: next, Total: nil})
}
func (s *store) distributionTrace(ctx context.Context, id uuid.UUID, reference string, after *time.Time, key string, limit int) ([]DistributionTraceItem, error) {
	var requestHash []byte
	if reference != "" {
		hash := sha256.Sum256([]byte(reference))
		requestHash = hash[:]
	}
	rows, e := s.pool.Query(ctx, distributionTraceSQL, id, reference, after, key, limit+1, requestHash)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []DistributionTraceItem{}
	for rows.Next() {
		var item DistributionTraceItem
		if e = rows.Scan(&item.Kind, &item.ID, &item.State, &item.CreatedAt, &item.ErrorCode, &item.EventID, &item.OperationID, &item.CorrelationID, &item.CausationID, &item.LeadID, &item.ResultVersion, &item.ExternalEffectState, &item.Evidence, &item.Attempts, &item.MessageID); e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func distributionReadError(err error) error {
	var api apiError
	if errors.As(err, &api) {
		return err
	}
	return errUnavailable("distribution diagnostics source is unavailable")
}
