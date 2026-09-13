package adminread

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

type Dependencies struct {
	Pool           *pgxpool.Pool
	Timeout        time.Duration
	Token          string
	Logger         *slog.Logger
	Revision       string
	RuntimeCatalog func() any
	Bridge         *activitybridge.Bridge
}

type handler struct {
	store    *store
	token    string
	logger   *slog.Logger
	revision string
	runtime  func() any
	bridge   *activitybridge.Bridge
}

func Register(router chi.Router, deps Dependencies) {
	h := &handler{
		store:    &store{pool: deps.Pool, timeout: deps.Timeout},
		token:    deps.Token,
		logger:   deps.Logger,
		revision: deps.Revision,
		runtime:  deps.RuntimeCatalog,
		bridge:   deps.Bridge,
	}
	router.Use(h.authenticate)
	for _, route := range apicontract.AdminRoutes {
		router.Method(route.Method, route.Path, h.routeHandler(route))
	}
}

func (h *handler) routeHandler(route apicontract.Route) http.HandlerFunc {
	switch route.Path {
	case apicontract.AdminBackend.Path:
		return h.backend
	case apicontract.AdminAccounts.Path:
		return h.listAccounts
	case apicontract.AdminAccount.Path:
		return h.getAccount
	case apicontract.AdminInstallations.Path:
		return h.listInstallations
	case apicontract.AdminInstallation.Path:
		return h.getInstallation
	case apicontract.AdminInstallationJobs.Path:
		return h.listInstallationJobs
	case apicontract.AdminInstallationAudit.Path:
		return h.listInstallationAudit
	case apicontract.AdminInstallationDeliveries.Path:
		return h.listDeliveries
	case apicontract.AdminIntegrations.Path:
		return h.listIntegrations
	case apicontract.AdminIntegration.Path:
		return h.getIntegration
	case apicontract.AdminJobs.Path:
		return h.listJobs
	case apicontract.AdminJobsSummary.Path:
		return h.jobsSummary
	case apicontract.AdminJob.Path:
		return h.getJob
	case apicontract.AdminAudit.Path:
		return h.listAudit
	case apicontract.AdminActivitySettings.Path:
		return h.getActivitySettings
	case apicontract.AdminActivityStatus.Path:
		return h.getActivityStatus
	case apicontract.AdminActivityOperation.Path:
		return h.getActivityOperation
	case apicontract.AdminActivityPanels.Path:
		return h.listActivityPanels
	case apicontract.AdminActivityPanel.Path:
		return h.getActivityPanel
	case apicontract.AdminActivityEmployees.Path:
		return h.listActivityEmployees
	case apicontract.AdminLeadStatusRules.Path:
		return h.listLeadStatusRules
	case apicontract.AdminLeadStatusRuns.Path:
		return h.listLeadStatusRuns
	case apicontract.AdminStats.Path:
		return h.stats
	case apicontract.AdminStatsAccounts.Path:
		return h.statsAccounts
	default:
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, errNotFound("not found"))
		}
	}
}

func (h *handler) backend(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	if h.logger != nil {
		h.logger.Info("admin backend",
			"request_id", httpmiddleware.RequestIDFromContext(r.Context()),
			"actor", actorFrom(r.Context()),
		)
	}
	components := any(services.Components())
	if h.runtime != nil {
		components = map[string]any{
			"catalog": services.Components(),
			"runtime": h.runtime(),
		}
	}
	writeJSON(w, http.StatusOK, BackendResponse{
		Source: sourceCore, ObservedAt: observed, Backend: sourceCore,
		Revision: h.revision, ContractVersion: contractVersion,
		Capabilities: adminCapabilities, Components: components,
	})
}

func (h *handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	f, err := parseInstallationListFilter(r, true)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, next, total, err := h.store.listAccounts(r.Context(), f)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next, Total: total,
	})
}

func (h *handler) getAccount(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	accountID, err := parseAccountID(chi.URLParam(r, "account_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	item, err := h.store.getAccount(r.Context(), accountID, observed)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	item.Source = sourceCore
	item.ObservedAt = observed
	writeJSON(w, http.StatusOK, item)
}

func (h *handler) listInstallations(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	f, err := parseInstallationListFilter(r, false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, next, err := h.store.listInstallations(r.Context(), f)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next,
	})
}

func (h *handler) getInstallation(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	card, err := h.store.getInstallation(r.Context(), id, observed)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, InstallationResponse{
		Source: sourceCore, ObservedAt: observed,
		Installation: card.Installation, Authorization: card.Authorization,
		Webhook: card.Webhook, Grants: card.Grants, Activity: card.Activity,
	})
}

func (h *handler) listInstallationJobs(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.requireInstallation(r, id); err != nil {
		h.queryFailed(w, r, err)
		return
	}
	f, err := parseJobListFilter(r, &id, false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	h.respondJobs(w, r, f)
}

func (h *handler) listJobs(w http.ResponseWriter, r *http.Request) {
	f, err := parseJobListFilter(r, nil, true)
	if err != nil {
		writeError(w, r, err)
		return
	}
	h.respondJobs(w, r, f)
}

func (h *handler) respondJobs(w http.ResponseWriter, r *http.Request, f jobListFilter) {
	observed := time.Now().UTC()
	items, next, err := h.store.listJobs(r.Context(), f)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next,
	})
}

func (h *handler) getJob(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	job, err := h.store.getJob(r.Context(), id)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	attempts, err := h.store.listJobAttempts(r.Context(), id)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, JobResponse{
		Source: sourceCore, ObservedAt: observed, Job: job, Attempts: attempts,
	})
}

func (h *handler) jobsSummary(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	counts, err := h.store.jobsSummary(r.Context())
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, JobsSummaryResponse{
		Source: sourceCore, ObservedAt: observed, Counts: counts,
	})
}

func (h *handler) listInstallationAudit(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.requireInstallation(r, id); err != nil {
		h.queryFailed(w, r, err)
		return
	}
	f, err := parseAuditListFilter(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	f.InstallationID = &id
	h.respondAudit(w, r, f)
}

func (h *handler) listAudit(w http.ResponseWriter, r *http.Request) {
	f, err := parseAuditListFilter(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	h.respondAudit(w, r, f)
}

func (h *handler) respondAudit(w http.ResponseWriter, r *http.Request, f auditListFilter) {
	observed := time.Now().UTC()
	items, next, err := h.store.listAudit(r.Context(), f)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next,
	})
}

func (h *handler) listDeliveries(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := h.requireInstallation(r, id); err != nil {
		h.queryFailed(w, r, err)
		return
	}
	limit, err := parseDeliveriesLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx, cancel := h.store.withTimeout(r.Context())
	defer cancel()
	items, err := activitybridge.ListDeliveriesFiltered(ctx, h.store.pool, activitybridge.DeliveryFilter{
		InstallationID: id, Limit: limit, FailedOnly: false, NewestFirst: true,
	})
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if items == nil {
		items = []activitybridge.Delivery{}
	}
	writeJSON(w, http.StatusOK, DeliveriesResponse{
		Source: sourceCore, ObservedAt: observed, Items: items,
	})
}

func (h *handler) listIntegrations(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	limit, cursorTime, cursorID, err := parseListCursor(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, next, err := h.store.listIntegrations(r.Context(), limit, cursorTime, cursorID)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next,
	})
}

func (h *handler) getIntegration(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	item, err := h.store.getIntegration(r.Context(), id)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, IntegrationResponse{
		Source: sourceCore, ObservedAt: observed, Integration: item,
	})
}

func (h *handler) requireInstallation(r *http.Request, id uuid.UUID) error {
	exists, err := h.store.installationExists(r.Context(), id)
	if err != nil {
		return err
	}
	if !exists {
		return errNotFound("installation not found")
	}
	return nil
}

func (h *handler) queryFailed(w http.ResponseWriter, r *http.Request, err error) {
	var api apiError
	if asAPIError(err, &api) {
		writeError(w, r, err)
		return
	}
	if h.logger != nil {
		h.logger.Error("admin read query failed",
			"request_id", httpmiddleware.RequestIDFromContext(r.Context()),
			"error", err,
		)
	}
	writeError(w, r, err)
}

func asAPIError(err error, out *apiError) bool {
	if err == nil {
		return false
	}
	if api, ok := err.(apiError); ok {
		*out = api
		return true
	}
	return false
}

func parseInstallationListFilter(r *http.Request, accounts bool) (installationListFilter, error) {
	limit, cursorTime, cursorID, err := parseListCursor(r)
	if err != nil {
		return installationListFilter{}, err
	}
	integrationID, err := parseOptionalUUID(r.URL.Query().Get("integration_id"))
	if err != nil {
		return installationListFilter{}, err
	}
	f := installationListFilter{
		IntegrationID: integrationID,
		Status:        strings.TrimSpace(r.URL.Query().Get("status")),
		Limit:         limit,
		CursorTime:    cursorTime,
		CursorID:      cursorID,
	}
	if accounts {
		f.Query = normalizeAccountQuery(r.URL.Query().Get("q"))
		return f, nil
	}
	accountID, err := parseOptionalInt64(r.URL.Query().Get("account_id"))
	if err != nil {
		return installationListFilter{}, err
	}
	f.AccountID = accountID
	f.Domain = strings.TrimSpace(r.URL.Query().Get("domain"))
	f.WebhookStatus = strings.TrimSpace(r.URL.Query().Get("webhook_status"))
	return f, nil
}

func parseJobListFilter(r *http.Request, installationID *uuid.UUID, requireWindow bool) (jobListFilter, error) {
	limit, cursorTime, cursorID, err := parseListCursor(r)
	if err != nil {
		return jobListFilter{}, err
	}
	f := jobListFilter{
		InstallationID: installationID,
		Status:         strings.TrimSpace(r.URL.Query().Get("status")),
		Type:           strings.TrimSpace(r.URL.Query().Get("type")),
		Limit:          limit,
		CursorTime:     cursorTime,
		CursorID:       cursorID,
	}
	if requireWindow {
		since, err := parseJobsSince(r.URL.Query().Get("since"), time.Now().UTC())
		if err != nil {
			return jobListFilter{}, err
		}
		f.Since = &since
	}
	return f, nil
}

func parseAuditListFilter(r *http.Request) (auditListFilter, error) {
	limit, cursorTime, cursorID, err := parseListCursor(r)
	if err != nil {
		return auditListFilter{}, err
	}
	installationID, err := parseOptionalUUID(r.URL.Query().Get("installation_id"))
	if err != nil {
		return auditListFilter{}, err
	}
	return auditListFilter{
		InstallationID: installationID,
		ObjectType:     strings.TrimSpace(r.URL.Query().Get("object_type")),
		ObjectID:       strings.TrimSpace(r.URL.Query().Get("object_id")),
		Action:         strings.TrimSpace(r.URL.Query().Get("action")),
		Limit:          limit,
		CursorTime:     cursorTime,
		CursorID:       cursorID,
	}, nil
}

func parseListCursor(r *http.Request) (int, *time.Time, string, error) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		return 0, nil, "", err
	}
	at, id, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return 0, nil, "", err
	}
	if id == "" {
		return limit, nil, "", nil
	}
	return limit, &at, id, nil
}
