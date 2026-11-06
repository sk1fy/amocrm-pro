package admincrm

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sk1fy/amocrm-pro/internal/adminread"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

type handler struct {
	service *Service
	logger  *slog.Logger
	now     func() time.Time
}

// Router is the worker admin CRM listener. It reuses the admin listener bearer
// and X-Admin-Actor authentication; the actor reaches only the access log.
func Router(logger *slog.Logger, token string, service *Service) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{service: service, logger: logger, now: service.now}
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	router.Use(httpmiddleware.Recover(logger))
	router.Use(httpmiddleware.AccessLog(logger))
	router.Use(adminread.Authenticate(token))
	for _, route := range apicontract.AdminCRMRoutes {
		router.Method(route.Method, route.Path, http.HandlerFunc(h.users))
	}
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "route not found")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, "invalid_argument", "method not allowed")
	})
	return router
}

type connectionDTO struct {
	InstallationID  uuid.UUID `json:"installation_id"`
	IntegrationID   uuid.UUID `json:"integration_id"`
	IntegrationCode string    `json:"integration_code"`
}

type userDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
	IsAdmin  bool   `json:"is_admin"`
}

type usersResponse struct {
	Source            string         `json:"source"`
	ObservedAt        time.Time      `json:"observed_at"`
	AccountID         int64          `json:"account_id"`
	State             string         `json:"state"`
	RetryAfterSeconds *int64         `json:"retry_after_seconds"`
	Connection        *connectionDTO `json:"connection"`
	FreshUntil        *time.Time     `json:"fresh_until"`
	Items             []userDTO      `json:"items"`
	Total             *int           `json:"total"`
}

func (h *handler) users(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	accountID, ok := parseAccountID(chi.URLParam(r, "account_id"))
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_argument", "account_id must be a positive integer")
		return
	}
	if len(r.URL.Query()) > 0 {
		writeError(w, r, http.StatusBadRequest, "invalid_argument", "query parameters are not supported")
		return
	}
	snapshot, err := h.service.Users(r.Context(), accountID)
	response := usersResponse{Source: "core", ObservedAt: h.now(), AccountID: accountID}
	var readError *ReadError
	switch {
	case err == nil:
		total := len(snapshot.Users)
		fresh := snapshot.FreshUntil.UTC()
		response.State = StateOK
		response.ObservedAt = snapshot.FetchedAt.UTC()
		response.Connection = toConnection(snapshot.Connection)
		response.FreshUntil = &fresh
		response.Total = &total
		response.Items = make([]userDTO, 0, total)
		for _, user := range snapshot.Users {
			response.Items = append(response.Items, userDTO{ID: user.ID, Name: user.Name, IsActive: user.IsActive, IsAdmin: user.IsAdmin})
		}
	case errors.Is(err, ErrAccountNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "account not found")
		return
	case errors.Is(err, ErrNoAuthorizedConnection):
		response.State = StateNoAuthorizedConnection
	case errors.As(err, &readError):
		reason, status := amocrm.FailureDiagnostic(readError.Err)
		h.logger.WarnContext(r.Context(), "admin crm users unavailable",
			"request_id", httpmiddleware.RequestIDFromContext(r.Context()).String(),
			"installation_id", readError.Connection.InstallationID.String(),
			"state", readError.State, "reason_code", reason, "upstream_status", status)
		response.State = readError.State
		response.Connection = toConnection(readError.Connection)
		if readError.State == StateRateLimited && readError.RetryAfter > 0 {
			seconds := int64(math.Ceil(readError.RetryAfter.Seconds()))
			response.RetryAfterSeconds = &seconds
		}
	default:
		h.logger.ErrorContext(r.Context(), "admin crm users failed",
			"request_id", httpmiddleware.RequestIDFromContext(r.Context()).String(), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func toConnection(connection Connection) *connectionDTO {
	return &connectionDTO{InstallationID: connection.InstallationID, IntegrationID: connection.IntegrationID, IntegrationCode: connection.IntegrationCode}
}

func parseAccountID(raw string) (int64, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 || strconv.FormatInt(value, 10) != raw {
		return 0, false
	}
	return value, true
}

type errorEnvelope struct {
	Error errorFields `json:"error"`
}

type errorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" {
		requestID = httpmiddleware.RequestIDFromContext(r.Context()).String()
	}
	writeJSON(w, status, errorEnvelope{Error: errorFields{Code: code, Message: message, RequestID: requestID, Retryable: status >= 500}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
