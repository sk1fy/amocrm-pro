package activitybridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type shareActivity struct {
	serviceapi.Activity
	lookup    serviceapi.ShareLookup
	lookupErr error
	view      serviceapi.ViewerPanel
}

func (a *shareActivity) ResolveShare(_ context.Context, req serviceapi.ShareLookupRequest) (serviceapi.ShareLookup, error) {
	if a.lookupErr != nil {
		return serviceapi.ShareLookup{}, a.lookupErr
	}
	if req.ViewKey != "synthetic-view-key" {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return a.lookup, nil
}
func (a *shareActivity) ViewPanel(context.Context, serviceapi.Auth) (serviceapi.ViewerPanel, error) {
	return a.view, nil
}
func (a *shareActivity) ListPanels(context.Context, serviceapi.Auth) ([]serviceapi.ManagedPanel, error) {
	return []serviceapi.ManagedPanel{{Name: "managed"}}, nil
}

type sharePolicy struct {
	kind    string
	version int
}

func (p *sharePolicy) Issue(_ context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	p.kind = r.Kind
	p.version = r.ViewKeyVersion
	return serviceapi.Auth{Token: "delegated"}, nil
}
func (p *sharePolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return serviceapi.Principal{}, nil
}

func TestShareHTTPRejectsCrossCredentialAndUnknownKey(t *testing.T) {
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	product := &shareActivity{lookup: serviceapi.ShareLookup{Scope: scope, PanelID: uuid.New(), Enabled: true, ViewKeyVersion: 2}, view: serviceapi.ViewerPanel{Name: "Shift"}}
	policy := &sharePolicy{}
	bridge := New(nil, policy, product, nil)
	bridge.ConfigureShare([]string{"https://activity.example.invalid"}, "management-token")
	router := chi.NewRouter()
	bridge.RegisterShareHTTP(router)

	unknown := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/view/panel", nil)
	req.Header.Set("Authorization", "Bearer unknown-key")
	req.Header.Set("Origin", "https://activity.example.invalid")
	router.ServeHTTP(unknown, req)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown key=%d %s", unknown.Code, unknown.Body.String())
	}

	viewerOnManagement := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/activity/panels", nil)
	req.Header.Set("Authorization", "Bearer synthetic-view-key")
	req.Header.Set("X-Activity-Installation-Id", scope.InstallationID.String())
	req.Header.Set("X-Activity-Integration-Id", scope.IntegrationID.String())
	router.ServeHTTP(viewerOnManagement, req)
	if viewerOnManagement.Code != http.StatusUnauthorized {
		t.Fatalf("viewer on management=%d %s", viewerOnManagement.Code, viewerOnManagement.Body.String())
	}

	managementOnViewer := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/activity/view/panel", nil)
	req.Header.Set("Authorization", "Bearer management-token")
	req.Header.Set("Origin", "https://activity.example.invalid")
	router.ServeHTTP(managementOnViewer, req)
	if managementOnViewer.Code != http.StatusNotFound {
		t.Fatalf("management on viewer=%d %s", managementOnViewer.Code, managementOnViewer.Body.String())
	}

	ok := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/activity/view/panel", nil)
	req.Header.Set("Authorization", "Bearer synthetic-view-key")
	req.Header.Set("Origin", "https://activity.example.invalid")
	router.ServeHTTP(ok, req)
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), `"name":"Shift"`) || policy.kind != serviceapi.PrincipalKindViewer || policy.version != 2 {
		t.Fatalf("viewer panel=%d %s kind=%s", ok.Code, ok.Body.String(), policy.kind)
	}

	managed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/activity/panels", nil)
	req.Header.Set("Authorization", "Bearer management-token")
	req.Header.Set("X-Activity-Installation-Id", scope.InstallationID.String())
	req.Header.Set("X-Activity-Integration-Id", scope.IntegrationID.String())
	router.ServeHTTP(managed, req)
	if managed.Code != http.StatusOK || policy.kind != serviceapi.PrincipalKindOperator {
		t.Fatalf("management list=%d %s kind=%s", managed.Code, managed.Body.String(), policy.kind)
	}
	var page serviceapi.ManagedPanelPage
	if err := json.Unmarshal(managed.Body.Bytes(), &page); err != nil || len(page.Panels) != 1 {
		t.Fatalf("management page=%s err=%v", managed.Body.String(), err)
	}
}

func TestShareHTTPFailClosedWithoutOrigins(t *testing.T) {
	bridge := New(nil, &sharePolicy{}, &shareActivity{}, nil)
	router := chi.NewRouter()
	bridge.RegisterShareHTTP(router)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/activity/view/panel", nil)
	req.Header.Set("Authorization", "Bearer synthetic-view-key")
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty origins=%d %s", w.Code, w.Body.String())
	}
}
