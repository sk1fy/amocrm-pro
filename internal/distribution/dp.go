package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

// DPTriggerJobType is the durable worker job created for every accepted Digital
// Pipeline trigger. CRM I/O never happens on the HTTP path.
const DPTriggerJobType = "distribution.dp_trigger"

// DPInstallations resolves the scoped installation channel from a webhook key
// hash. It reuses the exact active-channel semantics of the webhook ingress.
type DPInstallations interface {
	FindActiveByWebhookKeyHash(context.Context, []byte) (installations.Installation, error)
}

// DPReceiver is the public Digital Pipeline webhook ingress. It authenticates,
// validates and redacts the payload, dedupes and enqueues a job, then returns a
// fast accepted response. The webhook key is never persisted or logged.
type DPReceiver struct {
	Pool          *pgxpool.Pool
	Jobs          *jobs.Store
	Installations DPInstallations
	Store         *Store
	Logger        *slog.Logger
	MaxBody       int64
}

type dpEnvelope struct {
	Event struct {
		Type     int    `json:"type"`
		TypeCode string `json:"type_code"`
		Data     struct {
			ID          int64  `json:"id"`
			ElementType int    `json:"element_type"`
			StatusID    int64  `json:"status_id"`
			PipelineID  int64  `json:"pipeline_id"`
			Direction   string `json:"direction_of_movement"`
		} `json:"data"`
		Time int64 `json:"time"`
	} `json:"event"`
	Action struct {
		Settings struct {
			Widget struct {
				Settings map[string]any `json:"settings"`
			} `json:"widget"`
		} `json:"settings"`
	} `json:"action"`
	Subdomain string `json:"subdomain"`
	AccountID int64  `json:"account_id"`
}

var errDPUnsupportedBody = errors.New("unsupported digital pipeline body")

// decodeDPEnvelope accepts the JSON body used by our own tooling and the
// application/x-www-form-urlencoded body amoCRM actually posts for Digital
// Pipeline custom webhooks. The form uses nested bracket keys
// (event[data][id], action[settings][widget][settings][key]) with string
// values. Decoding is strict: duplicate keys, structural conflicts, malformed
// brackets and non-integer ids are rejected (400) instead of silently taking
// the last value or defaulting to zero. JSON stays the primary format and
// auth/dedupe semantics are unchanged.
func decodeDPEnvelope(contentType string, raw []byte, env *dpEnvelope) error {
	if json.Unmarshal(raw, env) == nil {
		return nil
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if ct != "application/x-www-form-urlencoded" {
		return errDPUnsupportedBody
	}
	form, e := url.ParseQuery(string(raw))
	if e != nil {
		return e
	}
	nested, e := formNested(form)
	if e != nil {
		return e
	}
	if _, ok := formGetMap(nested, "event", "type"); !ok {
		if _, ok := formGetMap(nested, "action", "settings", "widget", "settings", "key"); !ok {
			return errDPUnsupportedBody
		}
	}
	readInt := func(dst *int64, path ...string) error {
		n, e := formInt64(nested, path...)
		if e != nil {
			return e
		}
		*dst = n
		return nil
	}
	var evType, elementType int64
	if e = readInt(&evType, "event", "type"); e != nil {
		return e
	}
	env.Event.Type = int(evType)
	env.Event.TypeCode = formString(nested, "event", "type_code")
	if e = readInt(&env.Event.Time, "event", "time"); e != nil {
		return e
	}
	if e = readInt(&env.Event.Data.ID, "event", "data", "id"); e != nil {
		return e
	}
	if e = readInt(&elementType, "event", "data", "element_type"); e != nil {
		return e
	}
	env.Event.Data.ElementType = int(elementType)
	if e = readInt(&env.Event.Data.StatusID, "event", "data", "status_id"); e != nil {
		return e
	}
	if e = readInt(&env.Event.Data.PipelineID, "event", "data", "pipeline_id"); e != nil {
		return e
	}
	env.Event.Data.Direction = formString(nested, "event", "data", "direction_of_movement")
	env.Subdomain = formString(nested, "subdomain")
	if e = readInt(&env.AccountID, "account_id"); e != nil {
		return e
	}
	settings := map[string]any{}
	if v := formString(nested, "action", "settings", "widget", "settings", "key"); v != "" {
		settings["key"] = v
	}
	if v := formString(nested, "action", "settings", "widget", "settings", "groupId"); v != "" {
		settings["groupId"] = v
	}
	env.Action.Settings.Widget.Settings = settings
	return nil
}

// formNested expands bracket-notation form keys into a nested map. It rejects
// duplicate keys, structural conflicts (a key used as both scalar and object)
// and malformed/unbalanced brackets.
func formNested(values url.Values) (map[string]any, error) {
	root := map[string]any{}
	for k, vs := range values {
		if len(vs) != 1 {
			return nil, fmt.Errorf("duplicate form key %q", k)
		}
		parts, e := splitFormKey(k)
		if e != nil {
			return nil, e
		}
		cur := root
		for i, p := range parts {
			if i == len(parts)-1 {
				if _, exists := cur[p]; exists {
					return nil, fmt.Errorf("duplicate form key %q", k)
				}
				cur[p] = vs[0]
				break
			}
			next, exists := cur[p]
			if !exists {
				m := map[string]any{}
				cur[p] = m
				cur = m
				continue
			}
			m, ok := next.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("structural conflict at %q", k)
			}
			cur = m
		}
	}
	return root, nil
}

func splitFormKey(k string) ([]string, error) {
	if k == "" {
		return nil, fmt.Errorf("malformed form key %q", k)
	}
	head := k
	if i := strings.IndexByte(k, '['); i >= 0 {
		head = k[:i]
	}
	if head == "" {
		return nil, fmt.Errorf("malformed form key %q", k)
	}
	parts := []string{head}
	rest := k[len(head):]
	for rest != "" {
		if rest[0] != '[' {
			return nil, fmt.Errorf("malformed form key %q", k)
		}
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return nil, fmt.Errorf("unbalanced bracket in %q", k)
		}
		seg := rest[1:end]
		if seg == "" {
			return nil, fmt.Errorf("empty segment in %q", k)
		}
		parts = append(parts, seg)
		rest = rest[end+1:]
	}
	return parts, nil
}

func formGetMap(m map[string]any, path ...string) (any, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func formString(m map[string]any, path ...string) string {
	v, _ := formGetMap(m, path...)
	s, _ := v.(string)
	return s
}

// formInt64 parses an integer that may arrive as a string (form encoding).
// A present-but-invalid value is an error, never a silent zero.
func formInt64(m map[string]any, path ...string) (int64, error) {
	v, ok := formGetMap(m, path...)
	if !ok {
		return 0, nil
	}
	switch t := v.(type) {
	case string:
		n, e := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if e != nil {
			return 0, fmt.Errorf("invalid integer at %q", strings.Join(path, "."))
		}
		return n, nil
	case int64:
		return t, nil
	case float64:
		return int64(t), nil
	}
	return 0, fmt.Errorf("invalid integer type at %q", strings.Join(path, "."))
}

func (h *DPReceiver) maxBody() int64 {
	if h.MaxBody > 0 {
		return h.MaxBody
	}
	return MaxBody
}

func dpIgnored(w http.ResponseWriter) {
	write(w, 202, map[string]string{"state": "ignored"})
}

// Receive is a fast, durable accept: auth + validate + redact + dedupe + enqueue.
func (h *DPReceiver) Receive(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	raw, e := io.ReadAll(io.LimitReader(r.Body, h.maxBody()+1))
	if e != nil || int64(len(raw)) > h.maxBody() {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var env dpEnvelope
	if e := decodeDPEnvelope(r.Header.Get("Content-Type"), raw, &env); e != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key, _ := env.Action.Settings.Widget.Settings["key"].(string)
	groupRaw, _ := env.Action.Settings.Widget.Settings["groupId"].(string)
	if key == "" {
		http.NotFound(w, r)
		return
	}
	hash := sha256.Sum256([]byte(key))
	installation, e := h.Installations.FindActiveByWebhookKeyHash(ctx, hash[:])
	dpScoped := false
	var dpGroup uuid.UUID
	var dpRevision int64
	if errors.Is(e, installations.ErrNotFound) {
		if h.Store == nil {
			http.NotFound(w, r)
			return
		}
		cred, ce := h.Store.FindDPCredentialByHash(ctx, hash[:])
		if ce != nil {
			http.NotFound(w, r)
			return
		}
		// Per-group credential: the installation is resolved from the
		// credential; the group/revision scope is enforced after parsing.
		installation = installations.Installation{ID: cred.InstallationID, AccountID: cred.AccountID, Status: "active"}
		dpScoped, dpGroup, dpRevision = true, cred.GroupID, cred.BindingRevision
	} else if e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	d := env.Event.Data
	if env.AccountID != installation.AccountID || d.ElementType != 2 || d.ID <= 0 || d.PipelineID <= 0 || d.StatusID <= 0 {
		http.NotFound(w, r)
		return
	}
	switch env.Event.Type {
	case 1, 14, 15:
	default:
		dpIgnored(w)
		return
	}
	// Only incoming entries start distribution; a move out of the trigger never does.
	if d.Direction == "went_from_trigger" {
		dpIgnored(w)
		return
	}
	groupID, e := uuid.Parse(groupRaw)
	if e != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if dpScoped {
		// A per-group credential is valid only for its own group while the
		// binding revision that issued it is still the active one.
		current, ce := h.Store.ActiveBindingRevision(ctx, installation.ID, installation.AccountID)
		if groupID != dpGroup || ce != nil || current != dpRevision {
			http.NotFound(w, r)
			return
		}
	}
	// Redacted evidence: the scoped key is intentionally absent.
	evidence, _ := json.Marshal(map[string]any{
		"eventType": env.Event.Type, "typeCode": env.Event.TypeCode,
		"leadId": d.ID, "pipelineId": d.PipelineID, "statusId": d.StatusID,
		"direction": d.Direction, "time": env.Event.Time,
	})
	dedupInput := strings.Join([]string{
		installation.ID.String(), strconv.FormatInt(installation.AccountID, 10),
		strconv.FormatInt(d.ID, 10), strconv.FormatInt(d.PipelineID, 10), strconv.FormatInt(d.StatusID, 10),
		strconv.Itoa(env.Event.Type), d.Direction, strconv.FormatInt(env.Event.Time, 10),
	}, "|")
	dedup := sha256.Sum256([]byte(dedupInput))
	inboxID := uuid.New()
	tx, e := h.Pool.Begin(ctx)
	if e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, e := tx.Exec(ctx, `INSERT INTO distribution_dp_inbox(id,dedup_key,installation_id,account_id,lead_id,pipeline_id,status_id,event_type,direction,group_id,occurred_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(dedup_key) DO NOTHING`,
		inboxID, dedup[:], installation.ID, installation.AccountID, d.ID, d.PipelineID, d.StatusID, env.Event.Type, d.Direction, groupID, env.Event.Time, evidence)
	if e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if tag.RowsAffected() == 0 {
		write(w, 200, map[string]string{"state": "duplicate"})
		return
	}
	job, e := h.Jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{InstallationID: &installation.ID, Type: DPTriggerJobType, ActorType: "system", ActorID: "distribution-dp", ResourceType: "distribution_dp_inbox", ResourceID: inboxID.String(), Priority: 5, Payload: map[string]any{"inboxId": inboxID.String()}, MaxAttempts: 12})
	if e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, e = tx.Exec(ctx, `UPDATE distribution_dp_inbox SET job_id=$2 WHERE id=$1`, inboxID, job.ID); e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if e = tx.Commit(ctx); e != nil {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.Logger != nil {
		h.Logger.Info("distribution dp trigger accepted", "installation_id", installation.ID, "lead_id", d.ID)
	}
	write(w, 202, map[string]string{"state": "accepted"})
}

// DPTriggerWorker consumes the durable inbox, reads the lead (retryable),
// normalizes and writes the outbox. It never trusts the HTTP payload for CRM
// state and never touches the webhook key.
type DPTriggerWorker struct {
	Store *Store
	CRM   SourceCRM
}

func (w *DPTriggerWorker) Handler(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
	var p struct {
		InboxID uuid.UUID `json:"inboxId"`
	}
	if job.InstallationID == nil || json.Unmarshal(job.Payload, &p) != nil || p.InboxID == uuid.Nil {
		return nil, jobs.Permanent("invalid_payload", ErrNotFound)
	}
	var (
		installationID uuid.UUID
		accountID      int64
		leadID         int64
		pipelineID     int64
		statusID       int64
		direction      string
		groupID        uuid.UUID
		state          string
		evidence       []byte
		occurredAt     int64
	)
	if e := w.Store.pool.QueryRow(ctx, `SELECT installation_id,account_id,lead_id,pipeline_id,status_id,direction,group_id,state,payload,occurred_at FROM distribution_dp_inbox WHERE id=$1`, p.InboxID).
		Scan(&installationID, &accountID, &leadID, &pipelineID, &statusID, &direction, &groupID, &state, &evidence, &occurredAt); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return json.RawMessage(`{"state":"missing"}`), nil
		}
		return nil, e
	}
	if state != "pending" {
		return json.RawMessage(`{"state":"already_processed"}`), nil
	}
	if installationID != *job.InstallationID {
		return nil, jobs.Permanent("installation_mismatch", ErrDenied)
	}
	tx, e := w.Store.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		companyID       uuid.UUID
		bindingID       uuid.UUID
		bindingRevision int64
		integrationID   uuid.UUID
	)
	e = tx.QueryRow(ctx, `SELECT b.company_id,b.id,b.revision,b.integration_id FROM distribution_bindings b JOIN installations i ON i.id=b.installation_id JOIN integrations p ON p.id=b.integration_id JOIN integration_services c ON c.integration_id=p.id AND c.service_code='lead-distribution' WHERE b.installation_id=$1 AND b.account_id=$2 AND b.state='active' AND i.status='active' AND p.status='active' AND c.enabled ORDER BY b.revision DESC LIMIT 1`, installationID, accountID).
		Scan(&companyID, &bindingID, &bindingRevision, &integrationID)
	if errors.Is(e, pgx.ErrNoRows) {
		if _, e = tx.Exec(ctx, `UPDATE distribution_dp_inbox SET state='ignored',processed_at=now() WHERE id=$1 AND state='pending'`, p.InboxID); e != nil {
			return nil, e
		}
		if e = tx.Commit(ctx); e != nil {
			return nil, e
		}
		return json.RawMessage(`{"state":"no_binding"}`), nil
	}
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	binding := Binding{ID: bindingID, CompanyID: companyID, InstallationID: installationID, IntegrationID: integrationID, AccountID: accountID, Revision: bindingRevision}
	if e = w.Store.Require(ctx, binding); e != nil {
		if errors.Is(e, ErrNotFound) {
			return nil, jobs.Permanent("binding_inactive", ErrDenied)
		}
		return nil, e
	}
	scope := AssignmentScope{CompanyID: companyID, InstallationID: installationID, IntegrationID: integrationID, AccountID: accountID, BindingID: bindingID, BindingRevision: bindingRevision}
	observation, e := w.Store.ObserveLead(ctx, w.CRM, scope, leadID)
	if e != nil {
		return nil, e
	}
	triggerGroup := groupID
	// Propagate the real Digital Pipeline event time. Missing/invalid time stays
	// nil on purpose: TeamOS treats an absent source time as unproven and fails
	// closed, so we never fabricate "now" as entry evidence.
	var sourceOccurredAt *time.Time
	if occurredAt > 0 {
		t := time.Unix(occurredAt, 0).UTC()
		sourceOccurredAt = &t
	}
	envelope := EventEnvelope{SchemaVersion: 1, MessageID: uuid.New(), Scope: scope, EventID: uuid.New(), SourceOccurredAt: sourceOccurredAt, ReceivedAt: time.Now().UTC(), EmittedAt: time.Now().UTC(), CorrelationID: uuid.New(), Event: SourceEvent{Kind: "lead.digital_pipeline_trigger", LeadID: leadID, After: observation.Snapshot, ObservationRevision: observation.ObservationRevision, TriggerEvidence: json.RawMessage(evidence), TriggerGroupID: &triggerGroup}}
	body, e := json.Marshal(envelope)
	if e != nil {
		return nil, e
	}
	scoped, e := json.Marshal(scope)
	if e != nil {
		return nil, e
	}
	tx, e = w.Store.pool.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedState string
	if e = tx.QueryRow(ctx, `SELECT state FROM distribution_dp_inbox WHERE id=$1 FOR UPDATE`, p.InboxID).Scan(&lockedState); e != nil {
		return nil, e
	}
	if lockedState != "pending" {
		return json.RawMessage(`{"state":"already_processed"}`), nil
	}
	if _, e = tx.Exec(ctx, `INSERT INTO distribution_event_outbox(message_id,scope,payload) VALUES($1,$2,$3)`, envelope.MessageID, scoped, body); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `UPDATE distribution_dp_inbox SET state='processed',processed_at=now() WHERE id=$1`, p.InboxID); e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	if direction == "went_from_trigger" {
		return json.RawMessage(`{"state":"ignored_direction"}`), nil
	}
	return json.RawMessage(`{"state":"outbox_persisted"}`), nil
}
