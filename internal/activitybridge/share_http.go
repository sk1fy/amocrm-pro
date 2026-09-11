package activitybridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

var (
	errCapacity = serviceapi.Fail(serviceapi.ResourceExhausted, "capacity exhausted")
	errNotFound = serviceapi.Fail(serviceapi.NotFound, "not found")
)

func ShareRoutes() []apicontract.Route {
	return []apicontract.Route{
		apicontract.ActivityViewPanel, apicontract.ActivityViewTimeline, apicontract.ActivityViewEmployee, apicontract.ActivityViewEvent,
		apicontract.ActivityManagedPanels, apicontract.ActivityManagedPanelCreate, apicontract.ActivityManagedPanel, apicontract.ActivityManagedPanelPatch,
		apicontract.ActivityManagedShareLink, apicontract.ActivityManagedEmployees,
	}
}

func (b *Bridge) RegisterShareHTTP(router chi.Router) {
	for _, entry := range []struct {
		route   apicontract.Route
		handler http.HandlerFunc
		viewer  bool
	}{
		{apicontract.ActivityViewPanel, b.ViewPanelHTTP, true},
		{apicontract.ActivityViewTimeline, b.ViewTimelineHTTP, true},
		{apicontract.ActivityViewEmployee, b.ViewEmployeeHTTP, true},
		{apicontract.ActivityViewEvent, b.ViewEventHTTP, true},
		{apicontract.ActivityManagedPanels, b.ListPanelsHTTP, false},
		{apicontract.ActivityManagedPanelCreate, b.CreatePanelHTTP, false},
		{apicontract.ActivityManagedPanel, b.GetManagedPanelHTTP, false},
		{apicontract.ActivityManagedPanelPatch, b.PatchPanelHTTP, false},
		{apicontract.ActivityManagedShareLink, b.RotateShareLinkHTTP, false},
		{apicontract.ActivityManagedEmployees, b.ListEmployeesHTTP, false},
	} {
		handler := b.observeSize(entry.route, entry.handler)
		if entry.viewer {
			handler = b.viewerAccess(handler)
		} else {
			handler = b.managementAccess(handler)
		}
		router.Method(entry.route.Method, entry.route.Path, handler)
	}
	seen := map[string]bool{}
	for _, route := range ShareRoutes() {
		if strings.HasPrefix(route.Path, "/api/v1/activity/view/") && !seen[route.Path] {
			router.Method(http.MethodOptions, route.Path, b.viewerAccess(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
			seen[route.Path] = true
		}
	}
}

func (b *Bridge) viewerAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(b.appOrigins) == 0 {
			writeShareError(w, errNotFound)
			return
		}
		if !b.allowViewerOrigin(w, r) {
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		key, err := bearerCredential(r)
		if err != nil {
			writeShareError(w, err)
			return
		}
		hash := sha256.Sum256([]byte(key))
		if b.viewerLimiter != nil && !b.viewerLimiter.Allow(hash) {
			w.Header().Set("Retry-After", "1")
			writeShareError(w, errCapacity)
			return
		}
		next.ServeHTTP(w, r.WithContext(withViewKey(r.Context(), key)))
	})
}

func (b *Bridge) managementAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.managementToken == "" || !timingSafeEqual(bearerOrEmpty(r), b.managementToken) {
			writeShareError(w, serviceapi.Fail(serviceapi.Unauthenticated, "management authentication required"))
			return
		}
		installation, err := headerUUID(r, "X-Activity-Installation-Id")
		if err != nil {
			writeShareError(w, err)
			return
		}
		integration, err := headerUUID(r, "X-Activity-Integration-Id")
		if err != nil {
			writeShareError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withManagementScope(r.Context(), serviceapi.Scope{InstallationID: installation, IntegrationID: integration})))
	})
}

func (b *Bridge) ViewPanelHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ViewPanel(ctx, auth)
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) ViewTimelineHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	query, err := decodeViewerQuery(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	query.Auth = auth
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ViewTimeline(ctx, query)
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) ViewEmployeeHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathInt64(r, "employeeId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	query, err := decodeViewerQuery(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	query.Auth = auth
	query.UserIDs = []int64{id}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ViewEmployee(ctx, query)
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) ViewEventHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueViewer(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	if r.URL.RawQuery != "" {
		writeShareError(w, serviceapi.Fail(serviceapi.InvalidArgument, "event detail accepts no query parameters"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ViewEvent(ctx, serviceapi.EventRequest{Auth: auth, EventID: chi.URLParam(r, "eventId")})
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) ListPanelsHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueOperator(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	panels, err := b.activity.ListPanels(ctx, auth)
	respond(w, http.StatusOK, serviceapi.ManagedPanelPage{Panels: panels}, shareErr(err))
}

func (b *Bridge) CreatePanelHTTP(w http.ResponseWriter, r *http.Request) {
	auth, commandID, body, err := b.decodeManagedWrite(w, r, true)
	if err != nil {
		writeShareError(w, err)
		return
	}
	if body.Name == nil || body.EmployeeIDs == nil || body.DisplayWindow == nil {
		writeShareError(w, serviceapi.Fail(serviceapi.InvalidArgument, "name, employee_ids and display_window are required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.CreatePanel(ctx, serviceapi.PanelCommand{
		Auth: auth, CommandID: commandID, Name: *body.Name, EmployeeIDs: *body.EmployeeIDs,
		DisplayWindow: *body.DisplayWindow, Enabled: body.Enabled, HasName: true, HasEmployees: true, HasWindow: true,
	})
	respond(w, http.StatusCreated, result, shareErr(err))
}

func (b *Bridge) GetManagedPanelHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueOperator(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathUUID(r, "panelId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: id})
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) PatchPanelHTTP(w http.ResponseWriter, r *http.Request) {
	auth, _, body, err := b.decodeManagedWrite(w, r, false)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathUUID(r, "panelId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	command := serviceapi.PanelCommand{Auth: auth, PanelID: id, Revision: body.Revision, Enabled: body.Enabled}
	if body.Name != nil {
		command.Name = *body.Name
		command.HasName = true
	}
	if body.EmployeeIDs != nil {
		command.EmployeeIDs = *body.EmployeeIDs
		command.HasEmployees = true
	}
	if body.DisplayWindow != nil {
		command.DisplayWindow = *body.DisplayWindow
		command.HasWindow = true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.PatchPanel(ctx, command)
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) RotateShareLinkHTTP(w http.ResponseWriter, r *http.Request) {
	auth, commandID, _, err := b.decodeManagedWrite(w, r, true)
	if err != nil {
		writeShareError(w, err)
		return
	}
	id, err := pathUUID(r, "panelId")
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.RotateShareLink(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: commandID, PanelID: id, Rotate: true})
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) ListEmployeesHTTP(w http.ResponseWriter, r *http.Request) {
	auth, err := b.issueOperator(r)
	if err != nil {
		writeShareError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := b.activity.ListEmployees(ctx, auth)
	respond(w, http.StatusOK, result, shareErr(err))
}

func (b *Bridge) issueViewer(r *http.Request) (serviceapi.Auth, error) {
	key, ok := viewKeyFrom(r.Context())
	if !ok {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Unauthenticated, "authentication required")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	lookup, err := b.activity.ResolveShare(ctx, serviceapi.ShareLookupRequest{ViewKey: key})
	if err != nil {
		return serviceapi.Auth{}, shareErr(err)
	}
	if !lookup.Enabled {
		return serviceapi.Auth{}, errNotFound
	}
	return b.policy.Issue(ctx, serviceapi.IssueRequest{
		Scope: lookup.Scope, Kind: serviceapi.PrincipalKindViewer, PanelID: lookup.PanelID, ViewKeyVersion: lookup.ViewKeyVersion,
		Consumer: serviceapi.ActivityService, RequestID: requestID(r), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView),
	})
}

func (b *Bridge) issueOperator(r *http.Request) (serviceapi.Auth, error) {
	scope, ok := managementScopeFrom(r.Context())
	if !ok {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Unauthenticated, "management authentication required")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	return b.policy.Issue(ctx, serviceapi.IssueRequest{
		Scope: scope, Kind: serviceapi.PrincipalKindOperator, Consumer: serviceapi.ActivityService,
		RequestID: requestID(r), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels),
	})
}

type panelWriteBody struct {
	Name          *string                   `json:"name"`
	EmployeeIDs   *[]int64                  `json:"employee_ids"`
	DisplayWindow *serviceapi.DisplayWindow `json:"display_window"`
	Enabled       *bool                     `json:"enabled"`
	Revision      int64                     `json:"revision"`
}

func (b *Bridge) decodeManagedWrite(w http.ResponseWriter, r *http.Request, requireKey bool) (serviceapi.Auth, string, panelWriteBody, error) {
	auth, err := b.issueOperator(r)
	if err != nil {
		return serviceapi.Auth{}, "", panelWriteBody{}, err
	}
	var commandID string
	if requireKey {
		commandID, err = uuidIdempotencyKey(r)
		if err != nil {
			return serviceapi.Auth{}, "", panelWriteBody{}, err
		}
	}
	if r.Method == http.MethodPost && r.URL.Path != apicontract.ActivityManagedPanelCreate.Path && r.ContentLength == 0 {
		return auth, commandID, panelWriteBody{}, nil
	}
	var body panelWriteBody
	if err := decode(w, r, &body); err != nil {
		return serviceapi.Auth{}, "", panelWriteBody{}, err
	}
	return auth, commandID, body, nil
}

func decodeViewerQuery(r *http.Request) (serviceapi.Query, error) {
	values, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "malformed query parameters")
	}
	allowed := map[string]bool{"from": true, "to": true, "limit": true, "cursor": true, "buckets": true, "categories": true}
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
	query := serviceapi.Query{From: from, To: to, Limit: 100, Cursor: values.Get("cursor"), Buckets: values.Get("buckets")}
	if raw := values.Get("limit"); raw != "" {
		query.Limit, err = strconv.Atoi(raw)
		if err != nil || query.Limit < 1 {
			return serviceapi.Query{}, serviceapi.Fail(serviceapi.InvalidArgument, "limit must be 1..100")
		}
	}
	if raw := values.Get("categories"); raw != "" {
		query.Categories = strings.Split(raw, ",")
	}
	return query, serviceapi.ValidateQuery(query)
}

func bearerCredential(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", serviceapi.Fail(serviceapi.Unauthenticated, "authentication required")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", serviceapi.Fail(serviceapi.Unauthenticated, "authentication required")
	}
	return parts[1], nil
}

func bearerOrEmpty(r *http.Request) string {
	key, err := bearerCredential(r)
	if err != nil {
		return ""
	}
	return key
}

func timingSafeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func headerUUID(r *http.Request, name string) (uuid.UUID, error) {
	values := r.Header.Values(name)
	if len(values) != 1 {
		return uuid.Nil, serviceapi.Fail(serviceapi.InvalidArgument, "installation and integration headers are required")
	}
	id, err := uuid.Parse(values[0])
	if err != nil || id == uuid.Nil {
		return uuid.Nil, serviceapi.Fail(serviceapi.InvalidArgument, "installation and integration headers are required")
	}
	return id, nil
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return id, nil
}

func pathInt64(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return id, nil
}

func uuidIdempotencyKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", serviceapi.Fail(serviceapi.InvalidArgument, "invalid Idempotency-Key")
	}
	id, err := uuid.Parse(values[0])
	if err != nil || id == uuid.Nil {
		return "", serviceapi.Fail(serviceapi.InvalidArgument, "invalid Idempotency-Key")
	}
	return id.String(), nil
}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	return uuid.NewString()
}

func shareErr(err error) error {
	if err == nil {
		return nil
	}
	if serviceapi.ErrorCode(err) == serviceapi.NotFound {
		return errNotFound
	}
	return err
}

func writeShareError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	code := serviceapi.ErrorCode(err)
	if code == serviceapi.Unauthenticated {
		w.Header().Set("WWW-Authenticate", "Bearer")
		respond(w, http.StatusUnauthorized, jsonError{Error: jsonErrorFields{
			Code: string(code), Message: "authentication required",
			RequestID: w.Header().Get("X-Request-ID"), Retryable: false,
		}}, nil)
		return
	}
	writeError(w, err)
}

type viewKeyCtx struct{}
type managementScopeCtx struct{}

func withViewKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, viewKeyCtx{}, key)
}
func viewKeyFrom(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(viewKeyCtx{}).(string)
	return key, ok && key != ""
}
func withManagementScope(ctx context.Context, scope serviceapi.Scope) context.Context {
	return context.WithValue(ctx, managementScopeCtx{}, scope)
}
func managementScopeFrom(ctx context.Context) (serviceapi.Scope, bool) {
	scope, ok := ctx.Value(managementScopeCtx{}).(serviceapi.Scope)
	return scope, ok
}

func (b *Bridge) allowViewerOrigin(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if _, ok := b.appOrigins[origin]; !ok {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
	return true
}
