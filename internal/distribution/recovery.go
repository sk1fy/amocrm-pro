package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"net/http"
	"time"
)

type ScanCRM interface {
	SourceCRM
	DistributionLeadPage(context.Context, uuid.UUID, time.Time, time.Time, int) (amocrm.LeadScanPage, error)
}
type RecoveryScan struct {
	ID         uuid.UUID       `json:"scanId"`
	Scope      AssignmentScope `json:"scope"`
	From       time.Time       `json:"windowFrom"`
	To         time.Time       `json:"windowTo"`
	ItemCursor int             `json:"itemCursor"`
	Page       int             `json:"cursorPage"`
	MaxPages   int             `json:"maxPages"`
	State      string          `json:"state"`
	Gap        string          `json:"gapReason"`
	Error      *string         `json:"errorCode"`
}

func (h *Handler) startRecovery(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	var input struct {
		ID       uuid.UUID `json:"scanId"`
		From     time.Time `json:"windowFrom"`
		To       time.Time `json:"windowTo"`
		MaxPages int       `json:"maxPages"`
	}
	if decode(r, &input) != nil || input.ID == uuid.Nil || input.MaxPages < 1 || input.MaxPages > 20 || !input.To.After(input.From) || input.To.Sub(input.From) > 24*time.Hour || input.To.After(time.Now().Add(time.Second)) {
		fail(w, 400, "validation_failed")
		return
	}
	var created time.Time
	if e = h.Store.pool.QueryRow(r.Context(), `SELECT created_at FROM distribution_bindings WHERE id=$1`, b.ID).Scan(&created); e != nil {
		h.resultError(w, e)
		return
	}
	if input.From.Before(created) {
		fail(w, 400, "validation_failed")
		return
	}
	scope := AssignmentScope{CompanyID: b.CompanyID, InstallationID: b.InstallationID, IntegrationID: b.IntegrationID, AccountID: b.AccountID, BindingID: b.ID, BindingRevision: b.Revision}
	raw, _ := json.Marshal(scope)
	tx, e := h.Store.pool.Begin(r.Context())
	if e != nil {
		h.resultError(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	tag, e := tx.Exec(r.Context(), `INSERT INTO distribution_recovery_scans(id,binding_id,scope,window_from,window_to,max_pages) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, input.ID, b.ID, raw, input.From.UTC(), input.To.UTC(), input.MaxPages)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if tag.RowsAffected() > 0 {
		payload, _ := json.Marshal(map[string]any{"installation_id": b.InstallationID})
		if _, e = tx.Exec(r.Context(), `INSERT INTO jobs(installation_id,type,priority,payload) VALUES($1,'webhook.reconcile',10,$2)`, b.InstallationID, payload); e != nil {
			h.resultError(w, e)
			return
		}
	}
	var same bool
	var ownerBinding uuid.UUID
	if e = tx.QueryRow(r.Context(), `SELECT binding_id FROM distribution_recovery_scans WHERE id=$1`, input.ID).Scan(&ownerBinding); e != nil {
		h.resultError(w, e)
		return
	}
	if ownerBinding != b.ID {
		fail(w, 404, "resource_not_found")
		return
	}
	if e = tx.QueryRow(r.Context(), `SELECT binding_id=$2 AND window_from=$3 AND window_to=$4 AND max_pages=$5 FROM distribution_recovery_scans WHERE id=$1`, input.ID, b.ID, input.From, input.To, input.MaxPages).Scan(&same); e != nil || !same {
		fail(w, 409, "revision_conflict")
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 202, map[string]any{"scanId": input.ID, "state": "persisted"})
}
func (h *Handler) getRecovery(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	id, e := uuid.Parse(chi.URLParam(r, "scanId"))
	if e != nil {
		fail(w, 404, "resource_not_found")
		return
	}
	var scan RecoveryScan
	var raw []byte
	e = h.Store.pool.QueryRow(r.Context(), `SELECT id,scope,window_from,window_to,cursor_page,item_cursor,max_pages,state,gap_reason,error_code FROM distribution_recovery_scans WHERE id=$1 AND binding_id=$2`, id, b.ID).Scan(&scan.ID, &raw, &scan.From, &scan.To, &scan.Page, &scan.ItemCursor, &scan.MaxPages, &scan.State, &scan.Gap, &scan.Error)
	if errors.Is(e, pgx.ErrNoRows) {
		e = ErrNotFound
	}
	if e != nil {
		h.resultError(w, e)
		return
	}
	if json.Unmarshal(raw, &scan.Scope) != nil {
		fail(w, 503, "source_unavailable")
		return
	}
	write(w, 200, scan)
}

type RecoveryWorker struct {
	Store   *Store
	CRM     ScanCRM
	OnError func(string)
}

func (w *RecoveryWorker) active(ctx context.Context, scope AssignmentScope) error {
	b, e := w.Store.Get(ctx, Scope{CompanyID: scope.CompanyID, InstallationID: scope.InstallationID}, scope.BindingID)
	if e != nil {
		return e
	}
	if b.Revision != scope.BindingRevision || b.AccountID != scope.AccountID || b.IntegrationID != scope.IntegrationID {
		return ErrDenied
	}
	return w.Store.Require(ctx, b)
}
func (w *RecoveryWorker) Tick(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	token := uuid.New()
	var scan RecoveryScan
	var raw, idsRaw []byte
	var next *bool
	var index int
	e := w.Store.pool.QueryRow(ctx, `WITH pick AS(SELECT id FROM distribution_recovery_scans WHERE (state='pending' AND next_attempt_at<=clock_timestamp()) OR(state='scanning' AND lease_until<=clock_timestamp()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1) UPDATE distribution_recovery_scans s SET state='scanning',lease_token=$1,lease_until=clock_timestamp()+interval '30 seconds' FROM pick WHERE s.id=pick.id RETURNING s.id,s.scope,s.window_from,s.window_to,s.cursor_page,s.max_pages,s.page_ids,s.page_has_next,s.item_cursor`, token).Scan(&scan.ID, &raw, &scan.From, &scan.To, &scan.Page, &scan.MaxPages, &idsRaw, &next, &index)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if json.Unmarshal(raw, &scan.Scope) != nil {
		return w.failure(parent, token, "invalid_scope", true)
	}
	if e = w.active(ctx, scan.Scope); e != nil {
		return w.failure(parent, token, "binding_unavailable", true)
	}
	var ids []int64
	if idsRaw == nil {
		page, err := w.CRM.DistributionLeadPage(ctx, scan.Scope.InstallationID, scan.From, scan.To, scan.Page)
		if err != nil {
			return w.failure(parent, token, "scan_source_unavailable", false)
		}
		ids = page.IDs
		value := page.HasNext
		next = &value
		idsRaw, _ = json.Marshal(ids)
		tag, err := w.Store.pool.Exec(ctx, `UPDATE distribution_recovery_scans SET page_ids=$2,page_has_next=$3,item_cursor=0 WHERE lease_token=$1 AND lease_until>clock_timestamp()`, token, idsRaw, page.HasNext)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("scan lease lost")
		}
	} else if json.Unmarshal(idsRaw, &ids) != nil || next == nil {
		return w.failure(parent, token, "invalid_page", true)
	}
	for processed := 0; index < len(ids) && processed < 10; processed++ {
		if e = w.active(ctx, scan.Scope); e != nil {
			return w.failure(parent, token, "binding_unavailable", true)
		}
		id := ids[index]
		var exists bool
		if e = w.Store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_event_outbox WHERE recovery_scan_id=$1 AND recovery_lead_id=$2)`, scan.ID, id).Scan(&exists); e != nil {
			return e
		}
		var event EventEnvelope
		if !exists {
			o, err := w.Store.ObserveLead(ctx, w.CRM, scan.Scope, id)
			if err != nil {
				return w.failure(parent, token, "scan_source_unavailable", false)
			}
			event = EventEnvelope{SchemaVersion: 1, MessageID: uuid.New(), Scope: scan.Scope, EventID: uuid.New(), ReceivedAt: time.Now().UTC(), EmittedAt: time.Now().UTC(), CorrelationID: scan.ID, Event: SourceEvent{Kind: "lead.snapshot_reconciled", LeadID: id, After: o.Snapshot, ObservationRevision: o.ObservationRevision}}
		}
		tx, err := w.Store.pool.Begin(ctx)
		if err != nil {
			return err
		}
		var marker int
		err = tx.QueryRow(ctx, `SELECT 1 FROM distribution_recovery_scans WHERE lease_token=$1 AND state='scanning' AND lease_until>clock_timestamp() FOR UPDATE`, token).Scan(&marker)
		if err == nil {
			err = services.RequireEnabled(ctx, tx, scan.Scope.InstallationID, services.LeadDistribution, true)
		}
		var valid bool
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT state='active' AND company_id=$2 AND installation_id=$3 AND integration_id=$4 AND account_id=$5 AND revision=$6 FROM distribution_bindings WHERE id=$1 FOR SHARE`, scan.Scope.BindingID, scan.Scope.CompanyID, scan.Scope.InstallationID, scan.Scope.IntegrationID, scan.Scope.AccountID, scan.Scope.BindingRevision).Scan(&valid)
			if err == nil && !valid {
				err = ErrDenied
			}
		}
		if err == nil && !exists {
			body, _ := json.Marshal(event)
			scoped, _ := json.Marshal(scan.Scope)
			_, err = tx.Exec(ctx, `INSERT INTO distribution_event_outbox(message_id,scope,payload,recovery_scan_id,recovery_lead_id) SELECT $1,$2,$3,$5,$6 WHERE EXISTS(SELECT 1 FROM distribution_recovery_scans WHERE lease_token=$4 AND lease_until>clock_timestamp()) ON CONFLICT(recovery_scan_id,recovery_lead_id) DO NOTHING`, event.MessageID, scoped, body, token, scan.ID, id)
		}
		if err == nil {
			var tag pgconn.CommandTag
			tag, err = tx.Exec(ctx, `UPDATE distribution_recovery_scans SET item_cursor=$2,updated_at=clock_timestamp() WHERE lease_token=$1 AND lease_until>clock_timestamp()`, token, index+1)
			if err == nil && tag.RowsAffected() == 0 {
				err = errors.New("scan lease lost")
			}
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		index++
	}
	state, gap := "pending", "historical_transitions_unrecoverable"
	finishedPage := index == len(ids)
	if finishedPage && !(*next) {
		state = "completed"
	} else if finishedPage && scan.Page >= scan.MaxPages {
		state = "blocked"
		gap = "scan_page_limit"
	}
	_, e = w.Store.pool.Exec(ctx, `UPDATE distribution_recovery_scans SET cursor_page=cursor_page+CASE WHEN $4 THEN 1 ELSE 0 END,page_ids=CASE WHEN $4 THEN NULL ELSE page_ids END,page_has_next=CASE WHEN $4 THEN NULL ELSE page_has_next END,item_cursor=CASE WHEN $4 THEN 0 ELSE item_cursor END,state=$2,gap_reason=$3,lease_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE lease_token=$1 AND lease_until>clock_timestamp()`, token, state, gap, finishedPage)
	return e
}
func (w *RecoveryWorker) failure(parent context.Context, token uuid.UUID, code string, permanent bool) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	defer cancel()
	_, e := w.Store.pool.Exec(ctx, `UPDATE distribution_recovery_scans SET state=CASE WHEN $3 OR attempts>=19 THEN 'blocked' ELSE 'pending' END,attempts=attempts+1,error_code=$2,next_attempt_at=clock_timestamp()+interval '30 seconds',lease_token=NULL,lease_until=NULL,updated_at=clock_timestamp() WHERE lease_token=$1 AND lease_until>clock_timestamp()`, token, code, permanent)
	return e
}
func (w *RecoveryWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if e := w.Tick(ctx); e != nil && ctx.Err() == nil && w.OnError != nil {
			w.OnError("recovery_tick_failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (h *Handler) retryRecovery(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	id, e := uuid.Parse(chi.URLParam(r, "scanId"))
	if e != nil {
		fail(w, 404, "resource_not_found")
		return
	}
	var body struct {
		Page int `json:"expectedCursorPage"`
		Item int `json:"expectedItemCursor"`
	}
	if decode(r, &body) != nil || body.Page < 1 || body.Page > 20 || body.Item < 0 || body.Item > 250 {
		fail(w, 400, "validation_failed")
		return
	}
	tx, e := h.Store.pool.Begin(r.Context())
	if e != nil {
		h.resultError(w, e)
		return
	}
	defer tx.Rollback(r.Context())
	tag, e := tx.Exec(r.Context(), `UPDATE distribution_recovery_scans SET state='pending',attempts=0,error_code=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1 AND binding_id=$2 AND state='blocked' AND cursor_page=$3 AND item_cursor=$4 AND gap_reason<>'scan_page_limit'`, id, b.ID, body.Page, body.Item)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if tag.RowsAffected() != 1 {
		fail(w, 409, "revision_conflict")
		return
	}
	metadata, _ := json.Marshal(map[string]any{"expectedCursorPage": body.Page, "expectedItemCursor": body.Item, "keyId": scopeFrom(r).KeyID})
	if _, e = tx.Exec(r.Context(), `INSERT INTO audit_log(installation_id,actor_type,action,object_type,object_id,metadata) VALUES($1,'integration','distribution.scan.retry','distribution_scan',$2,$3)`, b.InstallationID, id.String(), metadata); e != nil {
		h.resultError(w, e)
		return
	}
	if e = tx.Commit(r.Context()); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 202, map[string]any{"scanId": id, "state": "persisted"})
}
func (h *Handler) deliveryStatus(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	var events, results map[string]int64 = map[string]int64{}, map[string]int64{}
	ctx := r.Context()
	for kind, table := range map[string]string{"events": "distribution_event_outbox", "results": "distribution_result_outbox"} {
		rows, err := h.Store.pool.Query(ctx, `SELECT state,count(*) FROM `+table+` WHERE payload->'scope'->>'bindingId'=$1 GROUP BY state`, b.ID.String())
		if err != nil {
			h.resultError(w, err)
			return
		}
		for rows.Next() {
			var state string
			var n int64
			if err = rows.Scan(&state, &n); err != nil {
				rows.Close()
				h.resultError(w, err)
				return
			}
			if kind == "events" {
				events[state] = n
			} else {
				results[state] = n
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			h.resultError(w, err)
			return
		}
	}
	var gaps int64
	if e = h.Store.pool.QueryRow(ctx, `SELECT count(*) FROM distribution_recovery_scans WHERE binding_id=$1 AND gap_reason<>''`, b.ID).Scan(&gaps); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"events": events, "results": results, "historicalGaps": gaps})
}
