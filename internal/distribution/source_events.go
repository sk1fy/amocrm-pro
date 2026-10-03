package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
)

const NormalizeEventJobType = "distribution.normalize_event"
const DeliveryConsumer = "teamos-distribution-v1"

type SourceCRM interface {
	GetLeadSnapshot(context.Context, uuid.UUID, int64) (amocrm.LeadState, error)
}
type SourceEvidence struct {
	PipelineID        *int64 `json:"pipelineId,string"`
	StatusID          *int64 `json:"statusId,string"`
	OldPipelineID     *int64 `json:"oldPipelineId,string"`
	OldStatusID       *int64 `json:"oldStatusId,string"`
	ResponsibleUserID *int64 `json:"responsibleUserId,string"`
}
type SourceEvent struct {
	SourceEvidence      *SourceEvidence `json:"sourceEvidence"`
	Kind                string          `json:"kind"`
	LeadID              int64           `json:"leadId,string"`
	EntryFingerprint    *string         `json:"entryFingerprint"`
	Before              *Snapshot       `json:"before"`
	After               *Snapshot       `json:"after"`
	ObservationRevision int64           `json:"observationRevision"`
}
type EventEnvelope struct {
	SchemaVersion    int             `json:"schemaVersion"`
	MessageID        uuid.UUID       `json:"messageId"`
	Scope            AssignmentScope `json:"scope"`
	EventID          uuid.UUID       `json:"eventId"`
	SourceEventID    *string         `json:"sourceEventId"`
	SourceOccurredAt *time.Time      `json:"sourceOccurredAt"`
	ReceivedAt       time.Time       `json:"receivedAt"`
	EmittedAt        time.Time       `json:"emittedAt"`
	CorrelationID    uuid.UUID       `json:"correlationId"`
	CausationID      *uuid.UUID      `json:"causationId"`
	Event            SourceEvent     `json:"event"`
}
type LeadObservation struct {
	LeadName            *string         `json:"leadName,omitempty"`
	LeadURL             *string         `json:"leadUrl,omitempty"`
	Scope               AssignmentScope `json:"scope"`
	LeadID              int64           `json:"leadId,string"`
	Snapshot            *Snapshot       `json:"snapshot"`
	Absent              bool            `json:"absent"`
	AbsenceReason       *string         `json:"absenceReason"`
	Deleted             bool            `json:"deleted"`
	ObservedAt          time.Time       `json:"observedAt"`
	ObservationRevision int64           `json:"observationRevision"`
}
type SourceWorker struct {
	Store    *Store
	Webhooks *webhook.Store
	CRM      SourceCRM
}

func (s *Store) observation(ctx context.Context) (int64, time.Time, error) {
	var revision int64
	var at time.Time
	e := s.pool.QueryRow(ctx, `SELECT nextval('distribution_observation_revision_seq'),clock_timestamp()`).Scan(&revision, &at)
	return revision, at.UTC(), e
}
func (s *Store) ObserveLead(ctx context.Context, crm SourceCRM, scope AssignmentScope, id int64) (LeadObservation, error) {
	o := LeadObservation{Scope: scope, LeadID: id}
	var e error
	o.ObservationRevision, o.ObservedAt, e = s.observation(ctx)
	if e != nil {
		return o, e
	}
	l, e := crm.GetLeadSnapshot(ctx, scope.InstallationID, id)
	if e != nil {
		if errors.Is(e, amocrm.ErrLeadAbsent) {
			o.Absent = true
			reason := "not_found_or_deleted"
			o.AbsenceReason = &reason
		} else if errors.Is(e, amocrm.ErrLeadDeleted) {
			o.Deleted = true
		} else {
			return o, e
		}
	} else {
		if l.ID != id || l.PipelineID <= 0 || l.StatusID <= 0 || l.ResponsibleUserID <= 0 || l.UpdatedAt <= 0 {
			return o, amocrm.ErrIncompleteResponse
		}
		snap := snapshotOf(l, o.ObservedAt)
		o.Snapshot = &snap
		if l.Name != "" {
			name := l.Name
			o.LeadName = &name
		}
	}
	raw, e := json.Marshal(o.Snapshot)
	if e != nil {
		return o, e
	}
	_, e = s.pool.Exec(ctx, `INSERT INTO distribution_source_heads(account_id,lead_id,observation_revision,snapshot,deleted,observed_at,absent,absence_reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(account_id,lead_id) DO UPDATE SET observation_revision=EXCLUDED.observation_revision,snapshot=EXCLUDED.snapshot,deleted=EXCLUDED.deleted,observed_at=EXCLUDED.observed_at,absent=EXCLUDED.absent,absence_reason=EXCLUDED.absence_reason WHERE distribution_source_heads.observation_revision<EXCLUDED.observation_revision`, scope.AccountID, id, o.ObservationRevision, raw, o.Deleted, o.ObservedAt, o.Absent, o.AbsenceReason)
	return o, e
}
func fieldID(m map[string]any, k string) int64 {
	v, ok := m[k].(string)
	if !ok {
		return 0
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n <= 0 {
		return 0
	}
	return n
}
func normalize(f webhook.FrozenConsumer, o LeadObservation) EventEnvelope {
	event := SourceEvent{Kind: "lead.snapshot_reconciled", LeadID: o.LeadID, After: o.Snapshot, ObservationRevision: o.ObservationRevision}
	var m map[string]any
	_ = json.Unmarshal(f.Event.Payload, &m)
	nullable := func(k string) *int64 {
		v := fieldID(m, k)
		if v <= 0 {
			return nil
		}
		return &v
	}
	event.SourceEvidence = &SourceEvidence{nullable("pipeline_id"), nullable("status_id"), nullable("old_pipeline_id"), nullable("old_status_id"), nullable("responsible_user_id")}
	// Only a documented event family supplies transition provenance. A generic
	// update, GET delta, or matching own PATCH does not establish an entry.
	switch f.Event.EventType {
	case "add":
		event.Kind = "lead.created"
	case "delete":
		if o.Absent || o.Deleted {
			event.Kind = "lead.deleted"
		}
	case "responsible":
		event.Kind = "lead.responsible_changed"
	case "status":
		old, status := fieldID(m, "old_status_id"), fieldID(m, "status_id")
		if old > 0 && status > 0 && old != status {
			event.Kind = "lead.status_changed"
		}

	}
	source := f.Event.ID.String()
	return EventEnvelope{1, uuid.New(), o.Scope, uuid.New(), &source, f.Event.EventAt, f.Event.ReceivedAt.UTC(), time.Now().UTC(), f.Event.ID, nil, event}
}
func (w *SourceWorker) Handler(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
	var p struct {
		ReceiptID uuid.UUID `json:"receiptId"`
	}
	if job.InstallationID == nil || json.Unmarshal(job.Payload, &p) != nil || p.ReceiptID == uuid.Nil {
		return nil, jobs.Permanent("invalid_payload", ErrNotFound)
	}
	frozen, e := w.Webhooks.ConsumerEvent(ctx, p.ReceiptID, *job.InstallationID)
	if e != nil {
		return nil, e
	}
	var state string
	if e = w.Store.pool.QueryRow(ctx, `SELECT state FROM webhook_consumer_receipts WHERE id=$1`, p.ReceiptID).Scan(&state); e != nil {
		return nil, e
	}
	if state != "pending" {
		return json.RawMessage(`{"state":"already_processed"}`), nil
	}
	var scope AssignmentScope
	if json.Unmarshal(frozen.Scope, &scope) != nil || scope.InstallationID != *job.InstallationID || frozen.Event.EntityID == nil || frozen.ConsumerID != webhook.DistributionConsumer || scope.BindingID == uuid.Nil || scope.AccountID <= 0 || !validRevision(scope.BindingRevision) {
		return nil, jobs.Permanent("invalid_scope", ErrDenied)
	}
	b, e := w.Store.Get(ctx, Scope{CompanyID: scope.CompanyID, InstallationID: scope.InstallationID}, scope.BindingID)
	if e != nil {
		return nil, e
	}

	active := b.Revision == scope.BindingRevision && b.IntegrationID == scope.IntegrationID && b.AccountID == scope.AccountID && b.State == "active"
	observed := active
	var envelope EventEnvelope
	if active {
		if e = w.Store.Require(ctx, b); e != nil {
			return nil, e
		}
		observation, err := w.Store.ObserveLead(ctx, w.CRM, scope, *frozen.Event.EntityID)
		if err != nil {
			return nil, err
		}
		envelope = normalize(frozen, observation)
	}
	body, e := json.Marshal(envelope)
	if e != nil {
		return nil, e
	}
	tx, e := w.Store.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if job.LockedBy == nil {
		return nil, jobs.ErrLeaseLost
	}
	var marker int
	if e = tx.QueryRow(ctx, `SELECT 1 FROM jobs WHERE id=$1 AND installation_id=$2 AND type=$3 AND status='processing' AND attempts=$4 AND locked_by=$5 AND locked_until>clock_timestamp() FOR SHARE`, job.ID, scope.InstallationID, NormalizeEventJobType, job.Attempts, *job.LockedBy).Scan(&marker); e != nil {
		return nil, jobs.ErrLeaseLost
	}
	var receiptJob uuid.UUID
	if e = tx.QueryRow(ctx, `SELECT state,job_id FROM webhook_consumer_receipts WHERE id=$1 AND installation_id=$2 AND consumer_id=$3 FOR UPDATE`, p.ReceiptID, scope.InstallationID, webhook.DistributionConsumer).Scan(&state, &receiptJob); e != nil {
		return nil, e
	}
	if receiptJob != job.ID {
		return nil, ErrDenied
	}
	if state == "pending" {

		if e = services.RequireEnabled(ctx, tx, scope.InstallationID, services.LeadDistribution, true); e != nil {
			if !errors.Is(e, services.ErrNotEnabled) {
				return nil, e
			}
			active = false
		}
		var exactActive bool
		if e = tx.QueryRow(ctx, `SELECT company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 AND state='active' FROM distribution_bindings WHERE id=$1 FOR SHARE`, scope.BindingID, scope.CompanyID, scope.InstallationID, scope.IntegrationID, scope.AccountID, scope.BindingRevision).Scan(&exactActive); e != nil {
			return nil, e
		}
		active = active && exactActive && observed

		if e = tx.QueryRow(ctx, `SELECT 1 FROM jobs WHERE id=$1 AND status='processing' AND attempts=$2 AND locked_by=$3 AND locked_until>clock_timestamp()`, job.ID, job.Attempts, *job.LockedBy).Scan(&marker); e != nil {
			return nil, jobs.ErrLeaseLost
		}
		final, disposition := "ignored", "obsolete_binding"
		if active {
			scoped, _ := json.Marshal(scope)
			if _, e = tx.Exec(ctx, `INSERT INTO distribution_event_outbox(message_id,consumer_receipt_id,scope,payload) VALUES($1,$2,$3,$4) ON CONFLICT(consumer_receipt_id) DO NOTHING`, envelope.MessageID, p.ReceiptID, scoped, body); e != nil {
				return nil, e
			}
			final, disposition = "processed", "outbox_persisted"
		}
		if _, e = tx.Exec(ctx, `UPDATE webhook_consumer_receipts SET state=$2,disposition=$3,finished_at=now() WHERE id=$1`, p.ReceiptID, final, disposition); e != nil {
			return nil, e
		}
	}

	e = tx.Commit(ctx)
	return json.RawMessage(`{"state":"persisted"}`), e
}
func (h *Handler) liveLead(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	id, e := strconv.ParseInt(chi.URLParam(r, "leadId"), 10, 64)
	if e != nil || id <= 0 {
		fail(w, 400, "validation_failed")
		return
	}
	crm, ok := h.CRM.(SourceCRM)
	if !ok {
		fail(w, 503, "source_unavailable")
		return
	}
	scope := AssignmentScope{CompanyID: b.CompanyID, InstallationID: b.InstallationID, IntegrationID: b.IntegrationID, AccountID: b.AccountID, BindingID: b.ID, BindingRevision: b.Revision}
	o, e := h.Store.ObserveLead(r.Context(), crm, scope, id)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	if o.Snapshot != nil {
		var domain string
		if e = h.Store.pool.QueryRow(r.Context(), `SELECT account_domain FROM installations WHERE id=$1 AND integration_id=$2 AND account_id=$3 AND status='active'`, b.InstallationID, b.IntegrationID, b.AccountID).Scan(&domain); e == nil {
			if base, e := amocrm.AccountBaseURL(domain); e == nil {
				u := base.ResolveReference(&url.URL{Path: "/leads/detail/" + strconv.FormatInt(id, 10)}).String()
				o.LeadURL = &u
			}
		}
	}
	write(w, 200, o)
}

func (w *SourceWorker) RegisterEvents(s *webhook.Store, handlers map[string]jobs.Handler) {
	for _, event := range []string{"add", "update", "status", "responsible", "delete"} {
		s.RegisterEventConsumer("leads", event, webhook.ConsumerDescriptor{ID: webhook.DistributionConsumer, JobType: NormalizeEventJobType})
	}
	handlers[NormalizeEventJobType] = w.Handler
}
