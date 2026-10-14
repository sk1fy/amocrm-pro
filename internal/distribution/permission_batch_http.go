package distribution

import (
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

func (h *Handler) permissionsBatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("bindingRevision") == "" {
		fail(w, 400, "validation_failed")
		return
	}
	b, err := h.binding(r)
	if err != nil {
		h.resultError(w, err)
		return
	}
	if err = h.Store.Require(r.Context(), b); err != nil {
		h.resultError(w, err)
		return
	}
	var body struct {
		EmployeeID uuid.UUID `json:"employeeId"`
		UserID     int64     `json:"userId,string"`
		LeadIDs    []string  `json:"leadIds"`
	}
	if decode(r, &body) != nil || body.UserID <= 0 || body.EmployeeID == uuid.Nil || len(body.LeadIDs) == 0 || len(body.LeadIDs) > 100 {
		fail(w, 400, "validation_failed")
		return
	}
	ids := make([]int64, 0, len(body.LeadIDs))
	seen := map[int64]bool{}
	for _, raw := range body.LeadIDs {
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id <= 0 || strconv.FormatInt(id, 10) != raw || seen[id] {
			fail(w, 400, "validation_failed")
			return
		}
		ids = append(ids, id)
		seen[id] = true
	}
	if err = h.Store.Mapped(r.Context(), b, body.EmployeeID, body.UserID); err != nil {
		h.resultError(w, err)
		return
	}
	results := CanViewLeads(r.Context(), h.CRM, b.InstallationID, body.UserID, ids)
	if err = h.Store.Require(r.Context(), b); err != nil {
		h.resultError(w, err)
		return
	}
	if err = h.Store.Mapped(r.Context(), b, body.EmployeeID, body.UserID); err != nil {
		h.resultError(w, err)
		return
	}
	// Unknown source state invalidates the whole page; never return an authoritative
	// partial count or disclose rows with an unconfirmed policy snapshot.
	var domain string
	_ = h.Store.pool.QueryRow(r.Context(), `SELECT account_domain FROM installations WHERE id=$1 AND integration_id=$2 AND account_id=$3 AND status='active'`, b.InstallationID, b.IntegrationID, b.AccountID).Scan(&domain)
	items := make([]map[string]any, 0, len(ids))
	checked := time.Now().UTC()
	for _, id := range ids {
		result, ok := results[id]
		if !ok || result.Err != nil {
			h.resultError(w, ErrUnavailable)
			return
		}
		item := map[string]any{"leadId": strconv.FormatInt(id, 10), "userId": strconv.FormatInt(body.UserID, 10), "canViewLead": result.Allowed, "reason": "current_crm_policy", "checkedAt": checked}
		if result.Allowed && result.Lead != nil {
			item["responsibleUserId"] = strconv.FormatInt(result.Lead.ResponsibleUserID, 10)
			if result.Lead.Name != "" && len(result.Lead.Name) <= 1024 {
				item["leadName"] = result.Lead.Name
			}
			if base, e := amocrm.AccountBaseURL(domain); e == nil {
				item["leadUrl"] = base.ResolveReference(&url.URL{Path: "/leads/detail/" + strconv.FormatInt(id, 10)}).String()
			}
		}
		items = append(items, item)
	}
	write(w, 200, map[string]any{"items": items})
}
