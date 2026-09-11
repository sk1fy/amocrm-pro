package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"reflect"
	"sync"
	"testing"
)

type parityRepository struct {
	mu       sync.Mutex
	settings serviceapi.Settings
	commands map[string]serviceapi.SettingsCommand
	panels   map[uuid.UUID]serviceapi.ManagedPanel
	shares   map[string]uuid.UUID
	receipts map[string]panelReceipt
}

type panelReceipt struct {
	hash   [32]byte
	kind   string
	panel  uuid.UUID
	issued string
}

func (r *parityRepository) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings, nil
}
func (r *parityRepository) Configure(_ context.Context, _ serviceapi.Principal, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.commands[c.CommandID]; ok && old.Settings != c.Settings {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "different settings")
	}
	r.commands[c.CommandID] = c
	r.settings = c.Settings
	return serviceapi.Operation{ID: c.CommandID, CommandID: c.CommandID, State: "succeeded"}, nil
}
func (r *parityRepository) Operation(_ context.Context, _ serviceapi.Principal, id string) (serviceapi.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.commands[id]; !ok {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return serviceapi.Operation{ID: id, CommandID: id, State: "succeeded"}, nil
}
func (r *parityRepository) ensure() {
	if r.panels == nil {
		r.panels = map[uuid.UUID]serviceapi.ManagedPanel{}
		r.shares = map[string]uuid.UUID{}
		r.receipts = map[string]panelReceipt{}
	}
}
func (r *parityRepository) ResolveShare(_ context.Context, hash []byte) (serviceapi.ShareLookup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	id, ok := r.shares[string(hash)]
	if !ok {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	panel := r.panels[id]
	return serviceapi.ShareLookup{Scope: serviceapi.Scope{}, PanelID: panel.ID, ViewKeyVersion: panel.ViewKeyVersion, Enabled: panel.Enabled, EmployeeIDs: append([]int64{}, panel.EmployeeIDs...)}, nil
}
func (r *parityRepository) CreatePanel(_ context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, panel serviceapi.ManagedPanel, hash []byte) (serviceapi.ManagedPanel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	if old, ok := r.receipts[c.CommandID]; ok {
		if old.kind != "create" {
			return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
		stored := r.panels[old.panel]
		stored.ViewKey = old.issued
		return stored, nil
	}
	if len(r.panels) >= serviceapi.MaxPanelsPerInstallation {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.ResourceExhausted, "at most 50 panels per installation")
	}
	panel.Revision = 1
	panel.ViewKeyVersion = 1
	panel.ShareUrlIssued = true
	r.panels[panel.ID] = panel
	r.shares[string(hash)] = panel.ID
	r.receipts[c.CommandID] = panelReceipt{kind: "create", panel: panel.ID, issued: panel.ViewKey}
	_ = p
	return panel, nil
}
func (r *parityRepository) ListPanels(context.Context, serviceapi.Scope) ([]serviceapi.ManagedPanel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	out := make([]serviceapi.ManagedPanel, 0, len(r.panels))
	for _, panel := range r.panels {
		out = append(out, panel)
	}
	return out, nil
}
func (r *parityRepository) GetPanel(_ context.Context, _ serviceapi.Scope, id uuid.UUID) (serviceapi.ManagedPanel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	panel, ok := r.panels[id]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return panel, nil
}
func (r *parityRepository) PatchPanel(_ context.Context, _ serviceapi.Principal, c serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	panel, ok := r.panels[c.PanelID]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if panel.Revision != c.Revision {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "revision mismatch")
	}
	if c.HasName {
		panel.Name = c.Name
	}
	if c.HasEmployees {
		panel.EmployeeIDs = append([]int64{}, c.EmployeeIDs...)
	}
	if c.HasWindow {
		panel.DisplayWindow = c.DisplayWindow
	}
	if c.Enabled != nil {
		panel.Enabled = *c.Enabled
	}
	panel.Revision++
	r.panels[c.PanelID] = panel
	return panel, nil
}
func (r *parityRepository) RotateShareLink(_ context.Context, _ serviceapi.Principal, c serviceapi.PanelCommand, hash []byte, viewKey string) (serviceapi.ManagedPanel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	if old, ok := r.receipts[c.CommandID]; ok {
		if old.kind != "rotate" || old.panel != c.PanelID {
			return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
		stored := r.panels[old.panel]
		stored.ViewKey = old.issued
		return stored, nil
	}
	panel, ok := r.panels[c.PanelID]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	for key, id := range r.shares {
		if id == c.PanelID {
			delete(r.shares, key)
		}
	}
	r.shares[string(hash)] = c.PanelID
	panel.ViewKeyVersion++
	panel.Revision++
	panel.ViewKey = viewKey
	panel.ShareUrlIssued = true
	r.panels[c.PanelID] = panel
	r.receipts[c.CommandID] = panelReceipt{kind: "rotate", panel: c.PanelID, issued: viewKey}
	return panel, nil
}

type parityEvents struct {
	serviceapi.CRMEvents
	policy serviceapi.Policy
}

func (e *parityEvents) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	if _, err := e.policy.Validate(ctx, q.Auth, "crm-events", "read"); err != nil {
		return serviceapi.QueryResult{}, err
	}
	return serviceapi.QueryResult{ReadVersion: serviceapi.PresentationReadVersion, Events: []serviceapi.Event{{ID: "event", CreatedAt: 100, CreatedBy: 7, Type: "lead_added", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)}}, Summaries: []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1, LastEventAt: 100}}, Totals: serviceapi.QueryTotals{UniqueEvents: 1, LastEventAt: 100}, Status: serviceapi.SyncStatus{State: "not_configured"}}, nil
}
func (e *parityEvents) GetEvent(context.Context, serviceapi.EventRequest) (serviceapi.Event, error) {
	return serviceapi.Event{ID: "event", CreatedAt: 100, CreatedBy: 7, Type: "lead_added", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)}, nil
}

func TestActivityBusinessLocalAndMTLSGRPCParity(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	events := &parityEvents{policy: corepolicy.ForCaller(policy, "crm-events")}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, "gateway"))
	local := product.New(repo, corepolicy.ForCaller(policy, "activity"), events, gw)
	address := start(t, ca, &Endpoints{Activity: local})
	remote := dialTest(t, ca, address, "core").Activity
	auth, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	query := serviceapi.Query{Auth: auth, From: 90, To: 200, UserIDs: []int64{7}, Limit: 100}
	localPanel, err := local.Panel(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	remotePanel, err := remote.Panel(ctx, query)
	if err != nil || !reflect.DeepEqual(localPanel, remotePanel) {
		t.Fatalf("local=%+v remote=%+v err=%v", localPanel, remotePanel, err)
	}
	if remotePanel.Coverage != "unknown" || len(remotePanel.Data.Events) != 1 || remotePanel.Data.Summaries[0].UniqueEvents != 1 {
		t.Fatalf("incorrect useful panel: %+v", remotePanel)
	}
	command := serviceapi.SettingsCommand{Auth: auth, CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 10}}
	accepted, err := remote.Configure(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := local.Configure(ctx, command)
	if err != nil || accepted != replay {
		t.Fatalf("local replay after remote commit=%+v err=%v", replay, err)
	}
	for _, client := range []serviceapi.Activity{local, remote} {
		settings, err := client.Settings(ctx, auth)
		if err != nil || settings != command.Settings {
			t.Fatalf("settings=%+v err=%v", settings, err)
		}
		op, err := client.Operation(ctx, serviceapi.OperationRequest{Auth: auth, OperationID: command.CommandID})
		if err != nil || op != accepted {
			t.Fatalf("operation=%+v err=%v", op, err)
		}
		bad := command
		bad.Settings.RetentionDays++
		if _, err := client.Configure(ctx, bad); serviceapi.ErrorCode(err) != serviceapi.Conflict {
			t.Fatalf("changed command=%v", err)
		}
		invalid := query
		invalid.To = invalid.From + 32*86400
		if _, err := client.Panel(ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("query bound=%v", err)
		}
		invalid = query
		invalid.Auth.Token = "forged"
		if _, err := client.Panel(ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
			t.Fatalf("forgery=%v", err)
		}
	}
	check.disabled.Store(true)
	for _, client := range []serviceapi.Activity{local, remote} {
		if _, err := client.Panel(ctx, query); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("disabled local/remote=%v", err)
		}
	}
}

func TestCreatePanelAndViewTimelineLocalAndMTLSParity(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	events := &parityEvents{policy: corepolicy.ForCaller(policy, "crm-events")}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, "gateway"))
	local := product.New(repo, corepolicy.ForCaller(policy, "activity"), events, gw).WithShareOrigin("https://activity.example.invalid")
	address := start(t, ca, &Endpoints{Activity: local})
	remote := dialTest(t, ca, address, "core").Activity
	operator, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindOperator, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels)})
	if err != nil {
		t.Fatal(err)
	}
	command := serviceapi.PanelCommand{Auth: operator, CommandID: uuid.NewString(), Name: "Shift A", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}}
	localPanel, err := local.CreatePanel(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	remotePanel, err := remote.CreatePanel(ctx, command)
	if err != nil || localPanel.ID != remotePanel.ID || localPanel.ViewKey != remotePanel.ViewKey || localPanel.ShareUrl != remotePanel.ShareUrl {
		t.Fatalf("create local=%+v remote=%+v err=%v", localPanel, remotePanel, err)
	}
	viewer, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindViewer, ViewKeyVersion: 1, PanelID: localPanel.ID, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)})
	if err != nil {
		t.Fatal(err)
	}
	query := serviceapi.Query{Auth: viewer, From: 90, To: 200, Limit: 100, Buckets: serviceapi.BucketAuto}
	localTimeline, err := local.ViewTimeline(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	remoteTimeline, err := remote.ViewTimeline(ctx, query)
	if err != nil || localTimeline.Timezone != remoteTimeline.Timezone || len(localTimeline.Data.Events) != len(remoteTimeline.Data.Events) {
		t.Fatalf("timeline local=%+v remote=%+v err=%v", localTimeline, remoteTimeline, err)
	}
}
