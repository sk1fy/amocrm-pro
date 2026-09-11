package corepolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"strings"
	"testing"
	"time"
)

type testChecker struct {
	scope                 serviceapi.Scope
	disabled, unavailable bool
	calls                 int
}

func (c *testChecker) Check(_ context.Context, s serviceapi.Scope, actor int64, system bool) error {
	c.calls++
	if c.unavailable {
		return serviceapi.Fail(serviceapi.Unavailable, "policy unavailable")
	}
	if c.disabled || s != c.scope || (!system && actor != 7) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "not admitted")
	}
	return nil
}
func testPolicy(t *testing.T) (*Service, *testChecker, serviceapi.IssueRequest) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &testChecker{scope: scope}
	s, err := NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	r := serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: "activity", RequestID: "test", Grants: serviceapi.UserGrants()}
	return s, check, r
}
func TestDelegationSignatureAudienceIdentityExpiryAndLiveRevocation(t *testing.T) {
	s, checker, r := testPolicy(t)
	core := ForCaller(s, "core")
	activity := ForCaller(s, "activity")
	auth, err := core.Issue(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	p, err := activity.Validate(context.Background(), auth, "activity", "panel")
	if err != nil || p.Scope != r.Scope || p.ActorID != 7 {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	cases := []struct {
		name                     string
		token                    serviceapi.Auth
		audience, action, caller string
		code                     serviceapi.Code
	}{{"forged", serviceapi.Auth{Token: strings.Repeat("x", len(auth.Token))}, "activity", "panel", "activity", serviceapi.Unauthenticated}, {"wrong audience", auth, "activity", "events", "activity", serviceapi.PermissionDenied}, {"wrong service identity", auth, "activity", "panel", "crm-events", serviceapi.PermissionDenied}, {"no identity", auth, "activity", "panel", "", serviceapi.PermissionDenied}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ForCaller(s, tt.caller).Validate(context.Background(), tt.token, tt.audience, tt.action)
			if serviceapi.ErrorCode(err) != tt.code {
				t.Fatalf("got %v want %s", err, tt.code)
			}
		})
	}
	checker.disabled = true
	if _, err := activity.Validate(context.Background(), auth, "activity", "panel"); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("revocation %v", err)
	}
	checker.disabled = false
	checker.unavailable = true
	if _, err := activity.Validate(context.Background(), auth, "activity", "panel"); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("outage %v", err)
	}
	checker.unavailable = false
	s.now = func() time.Time { return time.Now().Add(TokenLifetime + time.Second) }
	if _, err := activity.Validate(context.Background(), auth, "activity", "panel"); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
		t.Fatalf("expiry %v", err)
	}
}
func TestIssueRestrictsActorScopeAndCollectorGrants(t *testing.T) {
	s, _, r := testPolicy(t)
	cases := []struct {
		name, caller string
		mutate       func(*serviceapi.IssueRequest)
		want         serviceapi.Code
	}{{"nonadmin", "core", func(r *serviceapi.IssueRequest) { r.ActorID = 8 }, serviceapi.PermissionDenied}, {"other integration same installation", "core", func(r *serviceapi.IssueRequest) { r.IntegrationID = uuid.New() }, serviceapi.PermissionDenied}, {"other installation", "core", func(r *serviceapi.IssueRequest) { r.InstallationID = uuid.New() }, serviceapi.PermissionDenied}, {"Activity cannot issue", "activity", func(r *serviceapi.IssueRequest) {}, serviceapi.PermissionDenied}, {"collector cannot impersonate", "crm-events", func(r *serviceapi.IssueRequest) {}, serviceapi.PermissionDenied}, {"collector cannot read users", "crm-events", func(r *serviceapi.IssueRequest) {
		r.System = true
		r.ActorID = 0
		r.Grants = []serviceapi.Grant{{Audience: "gateway", Action: "users"}}
	}, serviceapi.PermissionDenied}, {"Core cannot drop original actor", "core", func(r *serviceapi.IssueRequest) { r.System = true; r.ActorID = 0 }, serviceapi.PermissionDenied}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := r
			tt.mutate(&req)
			_, err := ForCaller(s, tt.caller).Issue(context.Background(), req)
			if serviceapi.ErrorCode(err) != tt.want {
				t.Fatalf("got %v", err)
			}
		})
	}
	r.System = true
	r.ActorID = 0
	r.Grants = []serviceapi.Grant{{Audience: "gateway", Action: "events"}, {Audience: "crm-events", Action: "sync"}}
	auth, err := ForCaller(s, "crm-events").Issue(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ForCaller(s, "gateway").Validate(context.Background(), auth, "gateway", "events")
	if err != nil || !p.System {
		t.Fatalf("background %v %+v", err, p)
	}
	for _, action := range []string{serviceapi.ActionNotes, serviceapi.ActionTasks, serviceapi.ActionPipelines, serviceapi.ActionCustomFields, serviceapi.ActionEntities} {
		r.Grants = []serviceapi.Grant{{Audience: serviceapi.GatewayService, Action: action}}
		token, err := ForCaller(s, serviceapi.EventsService).Issue(context.Background(), r)
		if err != nil {
			t.Fatalf("collector issue %s: %v", action, err)
		}
		if _, err := ForCaller(s, serviceapi.GatewayService).Validate(context.Background(), token, serviceapi.GatewayService, action); err != nil {
			t.Fatalf("collector validate %s: %v", action, err)
		}
	}
}

func TestViewerAndOperatorIssueSkipRoleLookup(t *testing.T) {
	tracker := &issueTracker{scope: serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewWithChecker(tracker, key)
	if err != nil {
		t.Fatal(err)
	}
	core := ForCaller(s, serviceapi.CoreService)
	user := serviceapi.IssueRequest{Scope: tracker.scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "user", Grants: serviceapi.UserGrants()}
	if _, err := core.Issue(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if tracker.checks != 1 || tracker.delegations != 0 {
		t.Fatalf("widget path checks=%d delegations=%d", tracker.checks, tracker.delegations)
	}
	viewer := serviceapi.IssueRequest{Scope: tracker.scope, Kind: serviceapi.PrincipalKindViewer, ViewKeyVersion: 1, PanelID: uuid.New(), Consumer: serviceapi.ActivityService, RequestID: "viewer", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)}
	missingVersion := viewer
	missingVersion.ViewKeyVersion = 0
	if _, err := core.Issue(context.Background(), missingVersion); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("viewer without version accepted: %v", err)
	}
	auth, err := core.Issue(context.Background(), viewer)
	if err != nil {
		t.Fatal(err)
	}
	if tracker.checks != 1 || tracker.delegations != 1 {
		t.Fatalf("viewer looked up a role: checks=%d delegations=%d", tracker.checks, tracker.delegations)
	}
	p, err := ForCaller(s, serviceapi.ActivityService).Validate(context.Background(), auth, serviceapi.ActivityService, serviceapi.ActionView)
	if err != nil || p.Kind != serviceapi.PrincipalKindViewer || p.PanelID != viewer.PanelID || p.ViewKeyVersion != viewer.ViewKeyVersion || p.ActorID != 0 {
		t.Fatalf("viewer principal=%+v err=%v", p, err)
	}
	if _, err := ForCaller(s, serviceapi.ActivityService).Validate(context.Background(), auth, serviceapi.ActivityService, serviceapi.ActionPanel); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("viewer reached widget panel: %v", err)
	}
	operator := serviceapi.IssueRequest{Scope: tracker.scope, Kind: serviceapi.PrincipalKindOperator, Consumer: serviceapi.ActivityService, RequestID: "operator", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels)}
	if _, err := core.Issue(context.Background(), operator); err != nil {
		t.Fatal(err)
	}
	if tracker.checks != 1 || tracker.delegations != 3 {
		t.Fatalf("operator looked up a role: checks=%d delegations=%d", tracker.checks, tracker.delegations)
	}
	tracker.disabled = true
	if _, err := core.Issue(context.Background(), viewer); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("disabled pilot issued viewer: %v", err)
	}
	if _, err := core.Issue(context.Background(), serviceapi.IssueRequest{Scope: tracker.scope, System: true, ActorID: 0, Consumer: serviceapi.ActivityService, RequestID: "system", Grants: []serviceapi.Grant{{Audience: serviceapi.EventsService, Action: serviceapi.ActionSync}}}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("Core issued system: %v", err)
	}
}

type issueTracker struct {
	scope                 serviceapi.Scope
	disabled, unavailable bool
	checks, delegations   int
}

func (c *issueTracker) Check(_ context.Context, s serviceapi.Scope, actor int64, system bool) error {
	c.checks++
	if c.unavailable {
		return serviceapi.Fail(serviceapi.Unavailable, "policy unavailable")
	}
	if c.disabled || s != c.scope || (!system && actor != 7) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "not admitted")
	}
	return nil
}
func (c *issueTracker) CheckDelegation(_ context.Context, s serviceapi.Scope) error {
	c.delegations++
	if c.unavailable {
		return serviceapi.Fail(serviceapi.Unavailable, "policy unavailable")
	}
	if c.disabled || s != c.scope {
		return serviceapi.Fail(serviceapi.PermissionDenied, "not admitted")
	}
	return nil
}

func TestPanelDelegationCannotMutateOrInspectUnrelatedOperations(t *testing.T) {
	s, _, request := testPolicy(t)
	request.Grants = serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanel)
	auth, err := ForCaller(s, serviceapi.CoreService).Issue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range []serviceapi.Grant{
		{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSettings},
		{Audience: serviceapi.ActivityService, Action: serviceapi.ActionOperation},
		{Audience: serviceapi.EventsService, Action: serviceapi.ActionSync},
		{Audience: serviceapi.EventsService, Action: serviceapi.ActionOperation},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionEvents},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionNotes},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionTasks},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionPipelines},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionCustomFields},
		{Audience: serviceapi.GatewayService, Action: serviceapi.ActionEntities},
	} {
		if _, err := ForCaller(s, grant.Audience).Validate(context.Background(), auth, grant.Audience, grant.Action); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("panel delegation allows %+v: %v", grant, err)
		}
	}
}
