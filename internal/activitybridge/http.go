package activitybridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Routes() []apicontract.Route {
	return []apicontract.Route{apicontract.ActivityEvent, apicontract.ActivityPanel, apicontract.ActivityStatus, apicontract.ActivitySettings, apicontract.ActivityConfigure, apicontract.ActivitySync, apicontract.ActivityOperation}
}

// RegisterHTTP uses the existing JWT/CORS/rate middleware. Read middleware must
// consume its disposable JWT; command middleware defers consumption to admit.
func (b *Bridge) RegisterHTTP(router chi.Router, readMiddleware, commandMiddleware func(http.Handler) http.Handler) {
	for _, entry := range []struct {
		route   apicontract.Route
		handler http.HandlerFunc
		write   bool
	}{
		{apicontract.ActivityEvent, b.EventHTTP, false}, {apicontract.ActivityPanel, b.PanelHTTP, false}, {apicontract.ActivityStatus, b.StatusHTTP, false},
		{apicontract.ActivitySettings, b.SettingsHTTP, false}, {apicontract.ActivityConfigure, b.ConfigureHTTP, true},
		{apicontract.ActivitySync, b.SyncHTTP, true}, {apicontract.ActivityOperation, b.OperationHTTP, false},
	} {
		mw := readMiddleware
		if entry.write {
			mw = commandMiddleware
		}
		router.Method(entry.route.Method, entry.route.Path, mw(entry.handler))
	}
	seen := map[string]bool{}
	for _, route := range Routes() {
		if !seen[route.Path] {
			router.Method(http.MethodOptions, route.Path, commandMiddleware(http.NotFoundHandler()))
			seen[route.Path] = true
		}
	}
}

func (b *Bridge) EventHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeError(w, serviceapi.Fail(serviceapi.InvalidArgument, "event detail accepts no query parameters"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.GetEvent(ctx, p, chi.URLParam(r, "eventID"))
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) PanelHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	q, err := decodeQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Panel(ctx, p, q)
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) StatusHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Status(ctx, p)
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) SettingsHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Settings(ctx, p)
	respond(w, http.StatusOK, result, err)
}
func (b *Bridge) ConfigureHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var settings serviceapi.Settings
	if err := decode(w, r, &settings); err != nil {
		writeError(w, err)
		return
	}
	key, err := idempotencyKey(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Configure(ctx, p, key, settings)
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	respond(w, http.StatusAccepted, result, err)
}
func (b *Bridge) SyncHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var input SyncInput
	if err := decode(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	key, err := idempotencyKey(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Sync(ctx, p, key, input)
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	respond(w, http.StatusAccepted, result, err)
}
func (b *Bridge) OperationHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.Operation(ctx, p, chi.URLParam(r, "operationID"))
	respond(w, http.StatusOK, result, err)
}

func principal(w http.ResponseWriter, r *http.Request) (widgetauth.Principal, bool) {
	p, ok := widgetauth.PrincipalFromContext(r.Context())
	if !ok {
		writeError(w, serviceapi.Fail(serviceapi.Unauthenticated, "widget authentication required"))
	}
	return p, ok
}
func idempotencyKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !validKey(values[0]) {
		return "", serviceapi.Fail(serviceapi.InvalidArgument, "invalid Idempotency-Key")
	}
	return values[0], nil
}
func decode(w http.ResponseWriter, r *http.Request, into any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return serviceapi.Fail(serviceapi.InvalidArgument, "application/json required")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		return serviceapi.Fail(serviceapi.InvalidArgument, "one JSON object required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return serviceapi.Fail(serviceapi.InvalidArgument, "invalid request body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return serviceapi.Fail(serviceapi.InvalidArgument, "one JSON object required")
	}
	return nil
}
func decodeQuery(r *http.Request) (serviceapi.Query, error) {
	values, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "malformed query parameters")
	}
	allowed := map[string]bool{"from": true, "to": true, "user_ids": true, "limit": true, "cursor": true, "types": true, "type_prefix": true, "entity_type": true, "entity_ids": true, "order": true, "compact": true, "categories": true, "include_unknown_authors": true, "buckets": true, "group_id": true}
	for key, value := range values {
		if !allowed[key] || len(value) != 1 {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "unknown or repeated query parameter")
		}
	}
	from, err := strconv.ParseInt(values.Get("from"), 10, 64)
	if err != nil {
		return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "from must be Unix seconds")
	}
	to, err := strconv.ParseInt(values.Get("to"), 10, 64)
	if err != nil {
		return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "to must be Unix seconds")
	}
	query := serviceapi.Query{From: from, To: to, Limit: 100, Cursor: values.Get("cursor")}
	if raw := values.Get("limit"); raw != "" {
		query.Limit, err = strconv.Atoi(raw)
		if err != nil || query.Limit < 1 {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "limit must be 1..100")
		}
	}
	if raw := values.Get("user_ids"); raw != "" {
		seen := map[int64]bool{}
		for _, entry := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(entry, 10, 64)
			if err != nil || id <= 0 || seen[id] {
				return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "user_ids must be distinct positive IDs")
			}
			seen[id] = true
			query.UserIDs = append(query.UserIDs, id)
		}
	}
	if raw := values.Get("types"); raw != "" {
		query.Types = strings.Split(raw, ",")
	}
	query.TypePrefix = values.Get("type_prefix")
	query.EntityType = values.Get("entity_type")
	query.Order = values.Get("order")
	if raw := values.Get("entity_ids"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			id, err := strconv.ParseInt(entry, 10, 64)
			if err != nil {
				return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "entity_ids must be positive IDs")
			}
			query.EntityIDs = append(query.EntityIDs, id)
		}
	}
	if raw, present := values["compact"]; present {
		if raw[0] != "true" && raw[0] != "false" {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "compact must be true or false")
		}
		query.Compact = raw[0] == "true"
	}
	if raw := values.Get("categories"); raw != "" {
		query.Categories = strings.Split(raw, ",")
	}
	if raw, present := values["include_unknown_authors"]; present {
		if raw[0] != "true" && raw[0] != "false" {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "include_unknown_authors must be true or false")
		}
		query.IncludeUnknownAuthors = raw[0] == "true"
	}
	query.Buckets = values.Get("buckets")
	if raw := values.Get("group_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "group_id must be a positive ID")
		}
		query.GroupID = id
	}
	return query, serviceapi.ValidateQuery(query)
}
func respond(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	code := serviceapi.ErrorCode(err)
	if errors.Is(err, widgetauth.ErrReplay) {
		code = serviceapi.Unauthenticated
	}
	status := http.StatusServiceUnavailable
	switch code {
	case serviceapi.InvalidArgument:
		status = 400
	case serviceapi.Unauthenticated:
		status = 401
		w.Header().Set("WWW-Authenticate", "Bearer")
	case serviceapi.PermissionDenied, serviceapi.ReauthRequired:
		status = 403
	case serviceapi.NotFound:
		status = 404
	case serviceapi.Conflict:
		status = 409
	case serviceapi.ResourceExhausted:
		status = 429
	case serviceapi.DeadlineExceeded:
		status = 504
	}
	if status == 429 {
		w.Header().Set("Retry-After", "1")
	}
	respond(w, status, jsonError{Error: jsonErrorFields{
		Code: string(code), Message: safeErrorMessage(code),
		RequestID: w.Header().Get("X-Request-ID"), Retryable: retryableError(code),
	}}, nil)
}

type jsonError struct {
	Error jsonErrorFields `json:"error"`
}

type jsonErrorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func safeErrorMessage(code serviceapi.Code) string {
	switch code {
	case serviceapi.InvalidArgument:
		return "invalid request"
	case serviceapi.Unauthenticated:
		return "widget authentication required"
	case serviceapi.PermissionDenied:
		return "permission denied"
	case serviceapi.NotFound:
		return "not found"
	case serviceapi.Conflict:
		return "conflict"
	case serviceapi.Unavailable:
		return "temporarily unavailable"
	case serviceapi.DeadlineExceeded:
		return "request deadline exceeded"
	case serviceapi.ResourceExhausted:
		return "capacity exhausted"
	case serviceapi.ReauthRequired:
		return "installation requires authorization"
	default:
		return "internal error"
	}
}

func retryableError(code serviceapi.Code) bool {
	switch code {
	case serviceapi.Unavailable, serviceapi.ResourceExhausted, serviceapi.DeadlineExceeded:
		return true
	default:
		return false
	}
}
