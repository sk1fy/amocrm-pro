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
	} {
		if _, err := ForCaller(s, grant.Audience).Validate(context.Background(), auth, grant.Audience, grant.Action); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("panel delegation allows %+v: %v", grant, err)
		}
	}
}
