package activity

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type panelPolicy struct {
	principal serviceapi.Principal
}

func (p panelPolicy) Issue(context.Context, serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return serviceapi.Auth{Token: "ok"}, nil
}
func (p panelPolicy) Validate(_ context.Context, a serviceapi.Auth, _, _ string) (serviceapi.Principal, error) {
	if a.Token == "" {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.Unauthenticated, "invalid delegation")
	}
	return p.principal, nil
}

type panelEvents struct {
	serviceapi.CRMEvents
	events []serviceapi.Event
}

func (e *panelEvents) Query(_ context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	var matched []serviceapi.Event
	allow := map[int64]bool{}
	for _, id := range q.UserIDs {
		allow[id] = true
	}
	for _, event := range e.events {
		if allow[event.CreatedBy] {
			matched = append(matched, event)
		}
	}
	return serviceapi.QueryResult{ReadVersion: serviceapi.PresentationReadVersion, Events: matched, Summaries: []serviceapi.UserSummary{}, Totals: serviceapi.QueryTotals{UniqueEvents: int64(len(matched))}, Status: serviceapi.SyncStatus{VerifiedFrom: q.From, HistoryFrom: q.From, VerifiedThrough: q.To}}, nil
}
func (e *panelEvents) GetEvent(_ context.Context, req serviceapi.EventRequest) (serviceapi.Event, error) {
	for _, event := range e.events {
		if event.ID == req.EventID {
			return event, nil
		}
	}
	return serviceapi.Event{}, serviceapi.Fail(serviceapi.NotFound, "not found")
}
func (e *panelEvents) Status(context.Context, serviceapi.Auth) (serviceapi.SyncStatus, error) {
	return serviceapi.SyncStatus{}, nil
}

type panelGateway struct {
	serviceapi.Gateway
	users []serviceapi.User
}

func (g *panelGateway) Users(_ context.Context, r serviceapi.UsersRequest) (serviceapi.Directory, error) {
	if len(r.UserIDs) == 0 {
		return serviceapi.Directory{Users: g.users, Timezone: "Europe/Moscow"}, nil
	}
	allow := map[int64]bool{}
	for _, id := range r.UserIDs {
		allow[id] = true
	}
	var selected []serviceapi.User
	for _, user := range g.users {
		if allow[user.ID] {
			selected = append(selected, user)
		}
	}
	return serviceapi.Directory{Users: selected, Timezone: "Europe/Moscow"}, nil
}

func TestPostgresPanelsIsolationRotateDisableAndViewerDTO(t *testing.T) {
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("Activity integration test requires a _test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE panels,panel_commands`); err != nil {
		t.Fatal(err)
	}
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	other := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	store := NewPostgres(pool)
	events := &panelEvents{events: []serviceapi.Event{
		{ID: "in-panel", CreatedAt: 150, CreatedBy: 7, Type: "lead_added", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)},
		{ID: "outside", CreatedAt: 150, CreatedBy: 11, Type: "lead_added", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)},
	}}
	gw := &panelGateway{users: []serviceapi.User{{ID: 7, Name: "Alice", GroupID: 1, GroupName: "Sales"}, {ID: 9, Name: "Bob", GroupID: 1, GroupName: "Sales"}, {ID: 11, Name: "Eve"}}}
	operator := serviceapi.Principal{Scope: scope, Kind: serviceapi.PrincipalKindOperator}
	s := New(store, panelPolicy{principal: operator}, events, gw).WithShareOrigin("https://activity.example.invalid")
	auth := serviceapi.Auth{Token: "ok"}
	firstCommandID := uuid.NewString()
	first, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: firstCommandID, Name: "Shift A", EmployeeIDs: []int64{7, 9}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil || first.ViewKey == "" || first.ShareUrl == "" || first.Revision != 1 {
		t.Fatalf("create=%+v err=%v", first, err)
	}
	replay, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: firstCommandID, Name: "Shift A", EmployeeIDs: []int64{7, 9}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil || replay.ID != first.ID || replay.ViewKey != "" {
		t.Fatalf("replayed creation changed identity or exposed secret: id=%s err=%v", replay.ID, err)
	}
	commandID := uuid.NewString()
	created, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: commandID, Name: "Shift B", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "10:00", To: "19:00"}})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: commandID, Name: "Shift B", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "10:00", To: "19:00"}})
	if err != nil || again.ID != created.ID || again.ViewKey != "" || !again.ShareUrlIssued {
		t.Fatalf("idempotent create=%+v err=%v", again, err)
	}
	foreign := New(store, panelPolicy{principal: serviceapi.Principal{Scope: other, Kind: serviceapi.PrincipalKindOperator}}, events, gw)
	if _, err := foreign.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: first.ID}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("second installation saw panel: %v", err)
	}
	viewer := New(store, panelPolicy{principal: serviceapi.Principal{Scope: scope, Kind: serviceapi.PrincipalKindViewer, ViewKeyVersion: 1, PanelID: first.ID}}, events, gw)
	view, err := viewer.ViewPanel(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(view)
	for _, leak := range []string{first.ViewKey, "view_key", "revision", "installation", "retention"} {
		if strings.Contains(strings.ToLower(string(raw)), leak) && leak != first.ViewKey {
			t.Fatalf("viewer dto leaked %s: %s", leak, raw)
		}
	}
	if strings.Contains(string(raw), first.ViewKey) {
		t.Fatalf("viewer dto contained view key")
	}
	if _, err := viewer.ViewEvent(ctx, serviceapi.EventRequest{Auth: auth, EventID: "outside"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("outside event=%v", err)
	}
	if _, err := viewer.ViewEmployee(ctx, serviceapi.Query{Auth: auth, From: 100, To: 200, UserIDs: []int64{11}}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("outside employee=%v", err)
	}
	oldKey := first.ViewKey
	rotated, err := s.RotateShareLink(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), PanelID: first.ID})
	if err != nil || rotated.ViewKey == "" || rotated.ViewKey == oldKey {
		t.Fatalf("rotate=%+v err=%v", rotated, err)
	}
	if _, err := s.ResolveShare(ctx, serviceapi.ShareLookupRequest{ViewKey: oldKey}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("old key still valid: %v", err)
	}
	if _, err := s.ResolveShare(ctx, serviceapi.ShareLookupRequest{ViewKey: rotated.ViewKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.ViewPanel(ctx, auth); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("old version still views after rotate: %v", err)
	}
	viewer = New(store, panelPolicy{principal: serviceapi.Principal{Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: first.ID, ViewKeyVersion: rotated.ViewKeyVersion}}, events, gw)
	if _, err := viewer.ViewPanel(ctx, auth); err != nil {
		t.Fatalf("rotated viewer denied: %v", err)
	}
	disabled := false
	if _, err := s.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, PanelID: first.ID, Revision: rotated.Revision, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, PanelID: first.ID, Revision: rotated.Revision, Enabled: &disabled}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("stale revision=%v", err)
	}
	if lookup, err := s.ResolveShare(ctx, serviceapi.ShareLookupRequest{ViewKey: rotated.ViewKey}); err != nil || lookup.Enabled {
		t.Fatalf("disabled lookup=%+v err=%v", lookup, err)
	}
	if _, err := viewer.ViewPanel(ctx, auth); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("disabled view=%v", err)
	}
}

func TestPostgresConcurrentPanelQuota(t *testing.T) {
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires a _test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	s := New(NewPostgres(pool), panelPolicy{principal: serviceapi.Principal{Scope: scope, Kind: serviceapi.PrincipalKindOperator}}, nil, &panelGateway{users: []serviceapi.User{{ID: 7, Name: "User"}}})
	create := func() error {
		_, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: serviceapi.Auth{Token: "ok"}, CommandID: uuid.NewString(), Name: "Panel", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
		return err
	}
	for i := 0; i < 49; i++ {
		if err := create(); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func() { <-start; results <- create() }()
	}
	close(start)
	successes := 0
	for i := 0; i < 12; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
			t.Fatal(err)
		}
	}
	panels, err := s.store.ListPanels(ctx, scope)
	if err != nil || successes != 1 || len(panels) != 50 {
		t.Fatalf("successes=%d panels=%d err=%v", successes, len(panels), err)
	}
}
