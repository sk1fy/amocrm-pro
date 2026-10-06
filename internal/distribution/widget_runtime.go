package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// widgetUpstreamTimeout bounds a single Core->TeamOS widget call. The browser
// client budget is tighter than the public handler: widget client.js aborts at
// 11s (ajax) with a 12s watchdog, and ~0.3s (SDK disposable-token + TLS) plus
// ~1.5s of Core-side CanViewLead pre/post checks are spent outside this call.
// So the upstream budget must stay well under 11s: 0.3 + 1.5 + 9 = 10.8s worst
// case, i.e. Core always answers (200 or an honest 503) before the client gives
// up - the 503 is no longer masked by a client timeout. TeamOS `lead` is a
// 4-step fan-out (permission -> observation -> references -> permission) and
// varies 4.7-11.6s; raising this constant further would only shift the failure
// back into the browser. Auth/permission checks are unchanged.
const widgetUpstreamTimeout = 9 * time.Second

type widgetRuntimeRequest struct {
	Kind      string          `json:"kind"`
	ID        uuid.UUID       `json:"id"`
	GroupID   uuid.UUID       `json:"groupId"`
	LeadID    string          `json:"leadId"`
	Limit     int32           `json:"limit"`
	Offset    int32           `json:"offset"`
	Write     bool            `json:"write"`
	RequestID uuid.UUID       `json:"requestId"`
	Payload   json.RawMessage `json:"payload"`
}

func (h *Handler) teamWidgetCall(ctx context.Context, b Binding, path string, payload any) (int, json.RawMessage, error) {
	if h.TeamOSURL == "" || len(h.Keys[h.TeamOSKeyID]) < 32 {
		return 0, nil, ErrUnavailable
	}
	u, e := url.Parse(h.TeamOSURL + path)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return 0, nil, ErrUnavailable
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return 0, nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(raw))
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	Sign(req, Scope{h.TeamOSKeyID, b.CompanyID, b.InstallationID}, h.Keys[h.TeamOSKeyID], raw)
	client := &http.Client{Timeout: widgetUpstreamTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if h.HTTP != nil {
		copy := *h.HTTP
		copy.Timeout = widgetUpstreamTimeout
		copy.CheckRedirect = client.CheckRedirect
		client = &copy
	}
	resp, e := client.Do(req)
	if e != nil {
		return 0, nil, ErrUnavailable
	}
	defer resp.Body.Close()
	body, e := io.ReadAll(io.LimitReader(resp.Body, 262145))
	if e != nil || len(body) > 262144 || !json.Valid(body) {
		return 0, nil, ErrUnavailable
	}
	return resp.StatusCode, body, nil
}
func (h *Handler) widgetManage(ctx context.Context, b Binding, p widgetauth.Principal) (bool, error) {
	u, e := h.CRM.DistributionUser(ctx, b.InstallationID, p.UserID)
	if e != nil || u.ID != p.UserID || u.Rights.IsActive == nil || u.Rights.IsFree {
		return false, ErrUnavailable
	}
	return *u.Rights.IsActive && u.Rights.IsAdmin, nil
}
func (h *Handler) widgetRuntime(w http.ResponseWriter, r *http.Request) {
	b, p, e := h.widgetBinding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	var in widgetRuntimeRequest
	if decode(r, &in) != nil {
		fail(w, 400, "validation_failed")
		return
	}
	if in.Kind == "lead" || in.Kind == "history" || in.Kind == "action" {
		lead, e := strconv.ParseInt(in.LeadID, 10, 64)
		if e != nil || lead <= 0 || lead > 9007199254740991 || strconv.FormatInt(lead, 10) != in.LeadID {
			fail(w, 400, "validation_failed")
			return
		}
		allowed, e := CanViewLead(r.Context(), h.CRM, b.InstallationID, p.UserID, lead)
		if e != nil {
			h.resultError(w, e)
			return
		}
		if !allowed {
			fail(w, 403, "permission_denied")
			return
		}
	}
	if in.Write {
		allowed, e := h.widgetManage(r.Context(), b, p)
		if e != nil {
			h.resultError(w, e)
			return
		}
		if !allowed {
			fail(w, 403, "permission_denied")
			return
		}
		if in.RequestID == uuid.Nil {
			fail(w, 400, "validation_failed")
			return
		}
	}
	proofDeadline := p.TokenExpiresAt
	if maximum := time.Now().Add(15 * time.Minute); proofDeadline.After(maximum) {
		proofDeadline = maximum
	}
	payload := map[string]any{"companyId": b.CompanyID, "bindingId": b.ID, "bindingRevision": b.Revision, "installationId": b.InstallationID, "integrationId": b.IntegrationID, "accountId": strconv.FormatInt(b.AccountID, 10), "userId": strconv.FormatInt(p.UserID, 10), "principalExpiresAt": proofDeadline, "kind": in.Kind, "id": in.ID, "groupId": in.GroupID, "leadId": in.LeadID, "limit": in.Limit, "offset": in.Offset, "write": in.Write, "requestId": in.RequestID, "payload": in.Payload}
	status, body, e := h.teamWidgetCall(r.Context(), b, "/internal/v1/distribution/widget-runtime", payload)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.widgetPostDispatchError(w, in.Write, e)
		return
	}
	// Re-check mapping/section after the private request before exposing its result.
	allowed, e := h.localAccess(r.Context(), b, p, 0)
	if e != nil {
		h.widgetPostDispatchError(w, in.Write, e)
		return
	}
	if !allowed {
		h.widgetPostDispatchError(w, in.Write, ErrDenied)
		return
	}
	if !time.Now().Before(p.TokenExpiresAt) {
		h.widgetPostDispatchError(w, in.Write, widgetauth.ErrInvalidToken)
		return
	}
	if in.Write {
		allowed, e = h.widgetManage(r.Context(), b, p)
		if e != nil {
			h.widgetPostDispatchError(w, in.Write, e)
			return
		}
		if !allowed {
			h.widgetPostDispatchError(w, in.Write, ErrDenied)
			return
		}
	}
	if in.LeadID != "" {
		lead, _ := strconv.ParseInt(in.LeadID, 10, 64)
		allowed, e = CanViewLead(r.Context(), h.CRM, b.InstallationID, p.UserID, lead)
		if e != nil {
			h.widgetPostDispatchError(w, in.Write, e)
			return
		}
		if !allowed {
			h.widgetPostDispatchError(w, in.Write, ErrDenied)
			return
		}
	}
	switch status {
	case 200, 202, 400, 403, 404, 409, 503:
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	default:
		h.resultError(w, ErrUnavailable)
	}
}
func (h *Handler) widgetBootstrap(w http.ResponseWriter, r *http.Request) {
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		fail(w, 401, "unauthenticated")
		return
	}
	b, e := h.Store.ForPrincipal(r.Context(), p)
	if errors.Is(e, ErrNotFound) || errors.Is(e, ErrConflict) {
		state := "not_connected"
		if errors.Is(e, ErrConflict) {
			state = "ambiguous"
		}
		write(w, 200, map[string]any{"state": state, "accountId": strconv.FormatInt(p.AccountID, 10), "userId": strconv.FormatInt(p.UserID, 10), "canManage": false, "teamOSUrl": h.widgetTeamOSURL()})
		return
	}
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	payload := map[string]any{"companyId": b.CompanyID, "bindingId": b.ID, "bindingRevision": b.Revision, "installationId": b.InstallationID, "integrationId": b.IntegrationID, "accountId": strconv.FormatInt(b.AccountID, 10), "userId": strconv.FormatInt(p.UserID, 10)}
	status, raw, e := h.teamWidgetCall(r.Context(), b, "/internal/v1/distribution/widget-access", payload)
	if e != nil || status != 200 {
		h.resultError(w, ErrUnavailable)
		return
	}
	var access struct {
		Allowed    bool      `json:"allowed"`
		EmployeeID uuid.UUID `json:"employeeId"`
		CanManage  bool      `json:"canManage"`
	}
	if json.Unmarshal(raw, &access) != nil {
		h.resultError(w, ErrUnavailable)
		return
	}
	if !access.Allowed || access.EmployeeID == uuid.Nil {
		fail(w, 403, "permission_denied")
		return
	}
	if e = h.Store.Mapped(r.Context(), b, access.EmployeeID, p.UserID); e != nil {
		h.resultError(w, e)
		return
	}
	manage, e := h.widgetManage(r.Context(), b, p)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"binding": b, "state": b.State, "accountId": strconv.FormatInt(p.AccountID, 10), "userId": strconv.FormatInt(p.UserID, 10), "canManage": manage && access.CanManage, "teamOSUrl": h.widgetTeamOSURL()})
}

// After a write was dispatched, loss of authority cannot prove that its effect was absent.
func (h *Handler) widgetPostDispatchError(w http.ResponseWriter, writeRequest bool, e error) {
	if writeRequest {
		fail(w, 503, "outcome_unknown")
		return
	}
	h.resultError(w, e)
}

func (h *Handler) widgetTeamOSURL() any {
	u, e := url.Parse(h.TeamOSPublicURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil
	}
	return u.String() + "/distribution"
}
