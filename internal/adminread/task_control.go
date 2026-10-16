package adminread

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

// TaskControlDiagnostics is an explicit technical projection. Never marshal
// registry, credential, assertion or audit payload records into this response.
type TaskControlDiagnostics struct {
	Source                string     `json:"source"`
	ObservedAt            time.Time  `json:"observed_at"`
	FreshUntil            time.Time  `json:"fresh_until"`
	RequestID             uuid.UUID  `json:"request_id"`
	InstallationID        uuid.UUID  `json:"installation_id"`
	IntegrationID         uuid.UUID  `json:"integration_id"`
	AccountID             string     `json:"account_id"`
	Generation            int64      `json:"generation"`
	Revision              int64      `json:"revision"`
	InstallationState     string     `json:"installation_state"`
	GrantEnabled          bool       `json:"grant_enabled"`
	ProductEnabled        bool       `json:"product_enabled"`
	ReconciliationPending bool       `json:"reconciliation_pending"`
	NextCheckAt           *time.Time `json:"next_check_at,omitempty"`
	EvidenceCode          string     `json:"evidence_code"`
	RuntimeVersion        string     `json:"runtime_version,omitempty"`
	RuntimeConfirmedAt    *time.Time `json:"runtime_confirmed_at,omitempty"`
	RuntimeCompatible     bool       `json:"runtime_compatible"`
	DynamicRulesV2        bool       `json:"dynamic_rules_v2"`
	Code                  string     `json:"code"`
}

func (h *handler) taskControlDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		writeError(w, r, errInvalid("Запрос не должен содержать параметры или тело"))
		return
	}
	ctx, cancel := h.store.withTimeout(r.Context())
	defer cancel()
	now := time.Now().UTC()
	out := TaskControlDiagnostics{Source: sourceCore, ObservedAt: now, FreshUntil: now.Add(5 * time.Minute), RequestID: httpmiddleware.RequestIDFromContext(r.Context()), InstallationID: id}
	if out.RequestID == uuid.Nil {
		out.RequestID = uuid.New()
	}
	var account int64
	var state, productState, evidence, version string
	var enabled bool
	var next, confirmed *time.Time
	// One SQL statement gives a consistent registry/grant/generation snapshot.
	// EXISTS intentionally does not disclose service key IDs or company mappings.
	err = h.store.pool.QueryRow(ctx, `SELECT i.integration_id,i.account_id,i.status,p.status,
 COALESCE(s.enabled,false),COALESCE(v.generation,0),COALESCE(v.revision,0),
 COALESCE(v.evidence,'unknown'),COALESCE(v.signal_pending,false),v.next_check_at,
 EXISTS(SELECT 1 FROM task_control_service_grants g WHERE g.integration_id=i.integration_id AND g.account_id=i.account_id AND g.enabled),
 COALESCE(d.runtime_version,''),d.confirmed_at
 FROM installations i JOIN integrations p ON p.id=i.integration_id
 LEFT JOIN integration_services s ON s.integration_id=i.integration_id AND s.service_code='task-control'
 LEFT JOIN task_control_installation_versions v ON v.installation_id=i.id
 LEFT JOIN task_control_runtime_descriptors d ON d.installation_id=i.id AND d.generation=v.generation
 WHERE i.id=$1`, id).Scan(&out.IntegrationID, &account, &state, &productState, &enabled, &out.Generation, &out.Revision, &evidence, &out.ReconciliationPending, &next, &out.GrantEnabled, &version, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, r, errNotFound("Установка не найдена"))
		return
	}
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	out.AccountID = strconv.FormatInt(account, 10)
	out.ProductEnabled = enabled && productState == "active"
	out.InstallationState = "unknown"
	switch state {
	case "active", "disabled", "uninstalled":
		out.InstallationState = state
	}
	out.EvidenceCode = "unknown"
	switch evidence {
	case "registry", "disconnect_unverified", "disconnect_confirmed":
		out.EvidenceCode = evidence
	}
	if out.ReconciliationPending {
		out.NextCheckAt = next
	}
	// Stale-generation descriptors were excluded by the join above. Only an
	// active registry with confirmed evidence can advertise protocol compatibility.
	if state == "active" && out.ProductEnabled && out.GrantEnabled && !out.ReconciliationPending && evidence == "registry" {
		switch version {
		case "1.1.4", "1.2.0", "1.2.1", "1.2.2", "1.2.3":
			out.RuntimeVersion = version
			out.RuntimeConfirmedAt = confirmed
		}
		out.RuntimeCompatible = out.RuntimeVersion != ""
		out.DynamicRulesV2 = version == "1.2.0" || version == "1.2.1" || version == "1.2.2" || version == "1.2.3"
	}
	out.Code = "installation_unconfirmed"
	switch {
	case state == "uninstalled":
		out.Code = "operator_uninstall"
	case evidence == "disconnect_confirmed":
		out.Code = "disconnect_confirmed"
	case !out.ProductEnabled:
		out.Code = "product_disabled"
	case !out.GrantEnabled:
		out.Code = "grant_disabled"
	case out.ReconciliationPending:
		out.Code = "disconnect_unverified"
	case state != "active":
		out.Code = "installation_unconfirmed"
	case out.RuntimeVersion == "":
		out.Code = "runtime_unconfirmed"
	case !out.RuntimeCompatible:
		out.Code = "runtime_incompatible"
	default:
		out.Code = "ready"
	}
	w.Header().Set("X-Request-ID", out.RequestID.String())
	writeJSON(w, http.StatusOK, out)
}
