package adminread

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

func (h *handler) getActivitySettings(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("activity is unavailable"))
		return
	}
	settings, err := h.bridge.AdminSettings(r.Context(), scope, requestID(r))
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, ActivitySettingsResponse{
		Source: sourceCore, ObservedAt: observed,
		InitialDays: settings.InitialDays, RetentionDays: settings.RetentionDays,
		UpdatedAt: settings.UpdatedAt,
	})
}

func (h *handler) getActivityStatus(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("crm-events is unavailable"))
		return
	}
	status, err := h.bridge.AdminStatus(r.Context(), scope, requestID(r))
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, ActivityStatusResponse{
		Source: sourceCore, ObservedAt: observed,
		Enabled: status.Enabled, State: status.State, Verification: status.Verification,
		LagSeconds: status.LagSeconds, ReauthRequired: status.ReauthRequired, ErrorCode: status.ErrorCode,
		LastSuccessAt: unixRFC3339(status.LastSuccessAt), LastEventAt: unixRFC3339(status.LastEventAt),
		VerifiedFrom: unixRFC3339(status.VerifiedFrom), VerifiedThrough: unixRFC3339(status.VerifiedThrough),
	})
}

func (h *handler) getActivityOperation(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("activity is unavailable"))
		return
	}
	receipt, err := h.bridge.AdminOperation(r.Context(), scope, requestID(r), chi.URLParam(r, "operation_id"))
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, ActivityOperationResponse{
		Source: sourceCore, ObservedAt: observed,
		CommandID: receipt.CommandID, OperationID: receipt.OperationID,
		State: receipt.State, DeliveryState: receipt.DeliveryState, ErrorCode: receipt.ErrorCode,
	})
}

func (h *handler) listActivityPanels(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("activity is unavailable"))
		return
	}
	panels, err := h.bridge.AdminListPanels(r.Context(), scope, requestID(r))
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	items := make([]AdminPanel, 0, len(panels))
	for _, panel := range panels {
		items = append(items, publicPanel(panel))
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items,
	})
}

func (h *handler) getActivityPanel(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	panelID, err := parsePathUUID(chi.URLParam(r, "panel_id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("activity is unavailable"))
		return
	}
	panel, err := h.bridge.AdminGetPanel(r.Context(), scope, requestID(r), panelID)
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	item := publicPanel(panel)
	writeJSON(w, http.StatusOK, map[string]any{
		"source": sourceCore, "observed_at": observed, "panel": item,
		"id": item.ID, "name": item.Name, "employee_ids": item.EmployeeIDs,
		"display_window": item.DisplayWindow, "timezone": item.Timezone,
		"enabled": item.Enabled, "revision": item.Revision, "updated_at": item.UpdatedAt,
		"share_url_issued": item.ShareURLIssued,
	})
}

func (h *handler) listActivityEmployees(w http.ResponseWriter, r *http.Request) {
	observed := time.Now().UTC()
	scope, err := h.installationScope(r)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	if h.bridge == nil {
		writeError(w, r, errUnavailable("activity is unavailable"))
		return
	}
	directory, err := h.bridge.AdminListEmployees(r.Context(), scope, requestID(r))
	if err != nil {
		h.queryFailed(w, r, mapServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": sourceCore, "observed_at": observed,
		"items": directory.Users, "users": directory.Users, "timezone": directory.Timezone,
	})
}

func (h *handler) listLeadStatusRules(w http.ResponseWriter, r *http.Request) {
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
	items, err := h.store.listLeadStatusRules(r.Context(), id)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items,
	})
}

func (h *handler) listLeadStatusRuns(w http.ResponseWriter, r *http.Request) {
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
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursorTime, cursorID, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	items, next, err := h.store.listLeadStatusRuns(r.Context(), id, limit, cursorTime, cursorID)
	if err != nil {
		h.queryFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{
		Source: sourceCore, ObservedAt: observed, Items: items, NextCursor: next,
	})
}

func (h *handler) installationScope(r *http.Request) (serviceapi.Scope, error) {
	id, err := parsePathUUID(chi.URLParam(r, "id"))
	if err != nil {
		return serviceapi.Scope{}, err
	}
	scope, err := h.store.installationScope(r.Context(), id)
	if err != nil {
		return serviceapi.Scope{}, err
	}
	return scope, nil
}

func requestID(r *http.Request) string {
	return httpmiddleware.RequestIDFromContext(r.Context()).String()
}

func publicPanel(panel serviceapi.ManagedPanel) AdminPanel {
	ids := panel.EmployeeIDs
	if ids == nil {
		ids = []int64{}
	}
	return AdminPanel{
		ID: panel.ID.String(), Name: panel.Name, EmployeeIDs: ids,
		DisplayWindow: panel.DisplayWindow, Timezone: panel.Timezone,
		Enabled: panel.Enabled, Revision: panel.Revision, UpdatedAt: panel.UpdatedAt,
		ShareURLIssued: panel.ShareUrlIssued,
	}
}

func unixRFC3339(sec int64) *string {
	if sec == 0 {
		return nil
	}
	value := time.Unix(sec, 0).UTC().Format(time.RFC3339)
	return &value
}

func mapServiceError(err error) error {
	if err == nil {
		return nil
	}
	var api apiError
	if asAPIError(err, &api) {
		return err
	}
	switch serviceapi.ErrorCode(err) {
	case serviceapi.InvalidArgument:
		return errInvalid(safeServiceMessage(err, "invalid argument"))
	case serviceapi.Unauthenticated:
		return errUnauthenticated()
	case serviceapi.PermissionDenied, serviceapi.ReauthRequired:
		return errPermissionDenied(safeServiceMessage(err, "not permitted"))
	case serviceapi.NotFound:
		return errNotFound(safeServiceMessage(err, "not found"))
	case serviceapi.Conflict:
		return errConflict(safeServiceMessage(err, "conflict"))
	case serviceapi.Unavailable, serviceapi.DeadlineExceeded:
		return errUnavailable("backend unavailable")
	default:
		return err
	}
}

func safeServiceMessage(err error, fallback string) string {
	var se *serviceapi.Error
	if errors.As(err, &se) && se.Message != "" {
		return se.Message
	}
	return fallback
}
