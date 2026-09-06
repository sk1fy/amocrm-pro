package activity

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

type fakePolicy struct {
	deny  bool
	calls int
}

func (p *fakePolicy) Issue(context.Context, serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return serviceapi.Auth{Token: "verified"}, nil
}
func (p *fakePolicy) Validate(_ context.Context, a serviceapi.Auth, _, _ string) (serviceapi.Principal, error) {
	p.calls++
	if p.deny || a.Token != "verified" {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "denied")
	}
	return serviceapi.Principal{Scope: serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}, ActorID: 7}, nil
}

type memorySettings struct{ calls int }

func (m *memorySettings) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	m.calls++
	return Defaults(), nil
}
func (m *memorySettings) Configure(context.Context, serviceapi.Principal, serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	m.calls++
	return serviceapi.Operation{}, nil
}
func (m *memorySettings) Operation(context.Context, serviceapi.Principal, string) (serviceapi.Operation, error) {
	m.calls++
	return serviceapi.Operation{}, nil
}

type fakeEvents struct {
	serviceapi.CRMEvents
	calls  int
	query  serviceapi.Query
	status serviceapi.SyncStatus
}

func (e *fakeEvents) Query(_ context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	e.calls++
	e.query = q
	return serviceapi.QueryResult{Status: e.status, Summaries: []serviceapi.UserSummary{{UserID: 7}}}, nil
}

type fakeGateway struct {
	serviceapi.Gateway
	calls int
}

func (g *fakeGateway) Users(context.Context, serviceapi.UsersRequest) (serviceapi.Directory, error) {
	g.calls++
	return serviceapi.Directory{Users: []serviceapi.User{{ID: 7, Name: "Test"}, {ID: 9, Name: "Other"}}, Timezone: "Europe/Moscow"}, nil
}

func TestPanelUsesBoundedBatchAndExplicitCoverage(t *testing.T) {
	policy := &fakePolicy{}
	store := &memorySettings{}
	events := &fakeEvents{}
	gateway := &fakeGateway{}
	s := New(store, policy, events, gateway)
	query := serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200}
	panel, err := s.Panel(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if events.calls != 1 || gateway.calls != 1 || len(events.query.UserIDs) != 2 || events.query.Limit != 100 {
		t.Fatalf("unbounded/per-user calls: %+v %+v", events, gateway)
	}
	if panel.Coverage != "unknown" {
		t.Fatalf("missing history presented as %q", panel.Coverage)
	}
	for _, test := range []struct {
		status serviceapi.SyncStatus
		want   string
	}{
		{serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 150}, "partial"},
		{serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 200, LagSeconds: 601}, "stale"},
		{serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 200, ReauthRequired: true}, "stale"},
		{serviceapi.SyncStatus{VerifiedFrom: 100, HistoryFrom: 100, VerifiedThrough: 200}, "verified"},
	} {
		events.status = test.status
		panel, err = s.Panel(context.Background(), query)
		if err != nil || panel.Coverage != test.want {
			t.Fatalf("coverage=%s err=%v want %s", panel.Coverage, err, test.want)
		}
	}
}

func TestRevocationStopsLocalAdapterBeforeDataAccess(t *testing.T) {
	policy := &fakePolicy{deny: true}
	store := &memorySettings{}
	events := &fakeEvents{}
	gateway := &fakeGateway{}
	s := New(store, policy, events, gateway)
	auth := serviceapi.Auth{Token: "verified"}
	_, err := s.Panel(context.Background(), serviceapi.Query{Auth: auth, From: 1, To: 2})
	if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	_, err = s.Configure(context.Background(), serviceapi.SettingsCommand{Auth: auth, Settings: Defaults()})
	if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	_, err = s.Operation(context.Background(), serviceapi.OperationRequest{Auth: auth})
	if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	if events.calls+gateway.calls+store.calls != 0 {
		t.Fatal("denied local call accessed dependencies")
	}
}

func TestSettingsBounds(t *testing.T) {
	for _, s := range []serviceapi.Settings{{}, {InitialDays: 8, RetentionDays: 30}, {InitialDays: 7, RetentionDays: 2}, {InitialDays: 2, RetentionDays: 31}} {
		if ValidateSettings(s) == nil {
			t.Fatalf("accepted %+v", s)
		}
	}
	if err := ValidateSettings(Defaults()); err != nil {
		t.Fatal(err)
	}
}
