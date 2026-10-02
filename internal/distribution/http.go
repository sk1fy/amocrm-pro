package distribution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type WidgetVerifier interface {
	Verify(context.Context, string) (widgetauth.Principal, error)
}
type Handler struct {
	Store                  *Store
	CRM                    CRM
	Verifier               WidgetVerifier
	TeamOSURL, TeamOSKeyID string
	Keys                   map[string]string
	HTTP                   *http.Client
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, MaxBody+1))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra body")
	}
	return nil
}
func (h *Handler) resultError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, 404, "resource_not_found")
	case errors.Is(err, services.ErrNotEnabled):
		fail(w, 403, "capability_revoked")
	case errors.Is(err, ErrDenied):
		fail(w, 403, "permission_denied")
	case errors.Is(err, ErrConflict):
		fail(w, 409, "binding_mismatch")
	case errors.Is(err, widgetauth.ErrReplay):
		fail(w, 401, "unauthenticated")
	default:
		fail(w, 503, "source_unavailable")
	}
}
func (h *Handler) binding(r *http.Request) (Binding, error) {
	id, e := uuid.Parse(chi.URLParam(r, "bindingId"))
	if e != nil {
		return Binding{}, ErrNotFound
	}
	b, e := h.Store.Get(r.Context(), scopeFrom(r), id)
	if e != nil {
		return b, e
	}
	if rev := r.URL.Query().Get("bindingRevision"); rev != "" {
		v, err := strconv.ParseInt(rev, 10, 64)
		if err != nil || v != b.Revision {
			return b, ErrConflict
		}
	}
	return b, nil
}
func (h *Handler) RegisterService(router chi.Router, auth Auth) {
	router.Route("/internal/v1/distribution", func(r chi.Router) {
		r.Use(auth.Middleware)
		r.Post("/bindings", h.bind)
		r.Post("/assignments", h.admitAssignment)
		r.Get("/operations/{operationId}", h.getOperation)
		r.Post("/operations/{operationId}/cancel", h.cancelOperation)
		r.Post("/operations/{operationId}/reconcile", h.reconcileOperation)
		r.Get("/bindings/{bindingId}", h.getBinding)
		r.Get("/bindings/{bindingId}/readiness", h.readiness)
		r.Get("/bindings/{bindingId}/references", h.references)
		r.Get("/bindings/{bindingId}/leads/{leadId}", h.liveLead)
		r.Post("/bindings/{bindingId}/recovery-scans", h.startRecovery)
		r.Get("/bindings/{bindingId}/recovery-scans/{scanId}", h.getRecovery)
		r.Post("/bindings/{bindingId}/recovery-scans/{scanId}/retry", h.retryRecovery)
		r.Get("/bindings/{bindingId}/delivery-status", h.deliveryStatus)
		r.Put("/bindings/{bindingId}/mappings", h.mappings)
		r.Post("/bindings/{bindingId}/revoke", h.revoke)
		r.Post("/bindings/{bindingId}/permissions", h.permissions)
	})
}
func (h *Handler) bind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Binding
		WidgetToken string    `json:"widgetToken"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	if decode(r, &body) != nil {
		fail(w, 400, "validation_failed")
		return
	}
	scope := scopeFrom(r)
	if body.CompanyID != scope.CompanyID || body.InstallationID != scope.InstallationID {
		fail(w, 403, "permission_denied")
		return
	}
	// Resolve lost response before verifying/consuming the old disposable JWT.
	existing, err := h.Store.Get(r.Context(), scope, body.ID)
	if err == nil {
		if existing.IntentID == body.IntentID && existing.CompanyID == body.CompanyID && existing.InstallationID == body.InstallationID && existing.IntegrationID == body.IntegrationID && existing.AccountID == body.AccountID && existing.Revision == body.Revision {
			write(w, 200, existing)
		} else {
			fail(w, 409, "binding_mismatch")
		}
		return
	}
	if !errors.Is(err, ErrNotFound) {
		h.resultError(w, err)
		return
	}
	p, err := h.Verifier.Verify(r.Context(), body.WidgetToken)
	if err != nil {
		fail(w, 401, "unauthenticated")
		return
	}
	if p.InstallationID != scope.InstallationID {
		fail(w, 403, "permission_denied")
		return
	}
	if err = services.RequireEnabled(r.Context(), h.Store.pool, body.InstallationID, services.LeadDistribution, false); err != nil {
		h.resultError(w, err)
		return
	}
	u, err := h.CRM.DistributionUser(r.Context(), p.InstallationID, p.UserID)
	if err != nil {
		h.resultError(w, err)
		return
	}
	if u.Rights.IsActive == nil || !*u.Rights.IsActive || !u.Rights.IsAdmin {
		fail(w, 403, "permission_denied")
		return
	}
	b, err := h.Store.Bind(r.Context(), scope, body.Binding, p, body.ExpiresAt)
	if err != nil {
		h.resultError(w, err)
		return
	}
	write(w, 201, b)
}
func (h *Handler) getBinding(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, b)
}

type RefUser struct {
	ID       int64  `json:"id,string"`
	Name     string `json:"name"`
	IsActive bool   `json:"isActive"`
	GroupID  *int64 `json:"groupId,string"`
}
type RefStatus struct {
	ID   int64  `json:"id,string"`
	Name string `json:"name"`
}
type RefPipeline struct {
	ID       int64       `json:"id,string"`
	Name     string      `json:"name"`
	Statuses []RefStatus `json:"statuses"`
}

func (h *Handler) referenceData(ctx context.Context, b Binding) (any, error) {
	if err := h.Store.Require(ctx, b); err != nil {
		return nil, err
	}
	fetched := time.Now().UTC()
	users, err := h.CRM.DistributionUsers(ctx, b.InstallationID)
	if err != nil {
		return nil, err
	}
	pipelines, err := h.CRM.DistributionPipelines(ctx, b.InstallationID)
	if err != nil {
		return nil, err
	}
	// A source result obtained while capability was revoked is not exposed.
	if err = h.Store.Require(ctx, b); err != nil {
		return nil, err
	}
	us := []RefUser{}
	ps := []RefPipeline{}
	for _, u := range users {
		if u.Rights.IsActive == nil {
			return nil, ErrUnavailable
		}
		us = append(us, RefUser{u.ID, u.Name, *u.Rights.IsActive, u.Rights.GroupID})
	}
	for _, p := range pipelines {
		st := []RefStatus{}
		for _, s := range p.Statuses {
			st = append(st, RefStatus{s.ID, s.Name})
		}
		ps = append(ps, RefPipeline{p.ID, p.Name, st})
	}
	return map[string]any{"users": us, "pipelines": ps, "fetchedAt": fetched, "freshUntil": fetched.Add(5 * time.Minute), "state": "fresh"}, nil
}
func (h *Handler) references(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	v, e := h.referenceData(r.Context(), b)
	if e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, v)
}
func (h *Handler) mappings(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	var body struct {
		Mappings        []Mapping `json:"mappings"`
		MappingRevision int64     `json:"mappingRevision"`
	}
	if decode(r, &body) != nil || len(body.Mappings) > 1000 {
		fail(w, 400, "validation_failed")
		return
	}
	seenUser := map[int64]bool{}
	seenEmployee := map[uuid.UUID]bool{}
	users, e := h.CRM.DistributionUsers(r.Context(), b.InstallationID)
	if e != nil {
		h.resultError(w, e)
		return
	}
	valid := map[int64]bool{}
	for _, u := range users {
		valid[u.ID] = true
	}
	for _, m := range body.Mappings {
		if m.EmployeeID == uuid.Nil || m.UserID <= 0 || seenUser[m.UserID] || seenEmployee[m.EmployeeID] || !valid[m.UserID] {
			fail(w, 409, "mapping_required")
			return
		}
		seenUser[m.UserID] = true
		seenEmployee[m.EmployeeID] = true
	}
	if e = h.Store.ReplaceMappings(r.Context(), b, body.Mappings, body.MappingRevision); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"state": "synced", "count": len(body.Mappings), "mappingRevision": body.MappingRevision})
}
func (h *Handler) permissions(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	var body struct {
		EmployeeID uuid.UUID `json:"employeeId"`
		UserID     int64     `json:"userId,string"`
		LeadID     int64     `json:"leadId,string"`
	}
	if decode(r, &body) != nil || body.LeadID <= 0 {
		fail(w, 400, "validation_failed")
		return
	}
	if e = h.Store.Mapped(r.Context(), b, body.EmployeeID, body.UserID); e != nil {
		h.resultError(w, e)
		return
	}
	allowed, e := CanViewLead(r.Context(), h.CRM, b.InstallationID, body.UserID, body.LeadID)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Mapped(r.Context(), b, body.EmployeeID, body.UserID); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"userId": strconv.FormatInt(body.UserID, 10), "canViewLead": allowed, "reason": "current_crm_policy", "checkedAt": time.Now().UTC()})
}
func (h *Handler) RegisterWidget(router chi.Router, protect func(http.Handler) http.Handler, cors func(http.Handler) http.Handler) {
	router.Method("GET", "/api/v1/widget/distribution/bootstrap", protect(http.HandlerFunc(h.widgetBootstrap)))
	router.Method("POST", "/api/v1/widget/distribution/permissions", protect(http.HandlerFunc(h.widgetPermissions)))
	for _, path := range []string{"/api/v1/widget/distribution/bootstrap", "/api/v1/widget/distribution/permissions"} {
		router.Method("OPTIONS", path, cors(http.NotFoundHandler()))
	}
}
func (h *Handler) widgetBinding(r *http.Request) (Binding, widgetauth.Principal, error) {
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		return Binding{}, p, ErrDenied
	}
	b, e := h.Store.ForPrincipal(r.Context(), p)
	if e != nil {
		return b, p, e
	}
	e = h.Store.Require(r.Context(), b)
	return b, p, e
}
func (h *Handler) localAccess(ctx context.Context, b Binding, p widgetauth.Principal, leadID int64) (bool, error) {
	if h.TeamOSURL == "" || h.Keys[h.TeamOSKeyID] == "" {
		return false, ErrUnavailable
	}
	payload := map[string]any{"companyId": b.CompanyID, "bindingId": b.ID, "bindingRevision": b.Revision, "installationId": b.InstallationID, "integrationId": b.IntegrationID, "accountId": strconv.FormatInt(b.AccountID, 10), "userId": strconv.FormatInt(p.UserID, 10), "leadId": strconv.FormatInt(leadID, 10)}
	if leadID == 0 {
		delete(payload, "leadId")
	}
	body, _ := json.Marshal(payload)
	target := h.TeamOSURL + "/internal/v1/distribution/widget-access"
	u, e := url.Parse(target)
	if e != nil || u.Scheme != "https" || u.User != nil {
		return false, ErrUnavailable
	}
	req, e := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(body))
	if e != nil {
		return false, e
	}
	req.Header.Set("Content-Type", "application/json")
	Sign(req, Scope{h.TeamOSKeyID, b.CompanyID, b.InstallationID}, h.Keys[h.TeamOSKeyID], body)
	client := h.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, e := client.Do(req)
	if e != nil {
		return false, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, ErrUnavailable
	}
	var result struct {
		Allowed    bool      `json:"allowed"`
		EmployeeID uuid.UUID `json:"employeeId"`
		Reason     string    `json:"reason"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, MaxBody)).Decode(&result) != nil {
		return false, ErrUnavailable
	}
	if !result.Allowed || result.EmployeeID == uuid.Nil {
		return false, nil
	}
	if e = h.Store.Mapped(ctx, b, result.EmployeeID, p.UserID); e != nil {
		return false, e
	}
	return true, nil
}
func (h *Handler) widgetBootstrap(w http.ResponseWriter, r *http.Request) {
	b, p, e := h.widgetBinding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	allowed, e := h.localAccess(r.Context(), b, p, 0)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if !allowed {
		fail(w, 403, "permission_denied")
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"binding": b, "userId": strconv.FormatInt(p.UserID, 10), "state": b.State})
}
func (h *Handler) widgetPermissions(w http.ResponseWriter, r *http.Request) {
	b, p, e := h.widgetBinding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	var body struct {
		LeadID int64 `json:"leadId,string"`
	}
	if decode(r, &body) != nil || body.LeadID <= 0 {
		fail(w, 400, "validation_failed")
		return
	}
	local, e := h.localAccess(r.Context(), b, p, body.LeadID)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if !local {
		write(w, 200, map[string]any{"userId": strconv.FormatInt(p.UserID, 10), "canViewLead": false, "reason": "team_os_permission_denied", "checkedAt": time.Now().UTC()})
		return
	}
	allowed, e := CanViewLead(r.Context(), h.CRM, b.InstallationID, p.UserID, body.LeadID)
	if e != nil {
		h.resultError(w, e)
		return
	}
	if e = h.Store.Require(r.Context(), b); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"userId": strconv.FormatInt(p.UserID, 10), "canViewLead": allowed, "reason": "current_crm_policy", "checkedAt": time.Now().UTC()})
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	id, e := uuid.Parse(chi.URLParam(r, "bindingId"))
	if e != nil || id == uuid.Nil {
		fail(w, 400, "validation_failed")
		return
	}
	if e = h.Store.Revoke(r.Context(), scopeFrom(r), id); e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, map[string]any{"bindingId": id, "state": "revoked"})
}

func (h *Handler) readiness(w http.ResponseWriter, r *http.Request) {
	b, e := h.binding(r)
	if e != nil {
		h.resultError(w, e)
		return
	}
	v, e := h.Store.Readiness(r.Context(), b)
	if e != nil {
		h.resultError(w, e)
		return
	}
	write(w, 200, v)
}
