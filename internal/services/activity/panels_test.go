package activity

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type memoryPanels struct {
	memorySettings
	mu       sync.Mutex
	panels   map[uuid.UUID]serviceapi.ManagedPanel
	hashes   map[string]uuid.UUID
	receipts map[string]serviceapi.ManagedPanel
}

func (m *memoryPanels) init() {
	if m.panels == nil {
		m.panels = map[uuid.UUID]serviceapi.ManagedPanel{}
		m.hashes = map[string]uuid.UUID{}
		m.receipts = map[string]serviceapi.ManagedPanel{}
	}
}

func (m *memoryPanels) ResolveShare(_ context.Context, hash []byte) (serviceapi.ShareLookup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	id, ok := m.hashes[string(hash)]
	if !ok {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	panel := m.panels[id]
	return serviceapi.ShareLookup{PanelID: panel.ID, ViewKeyVersion: panel.ViewKeyVersion, Enabled: panel.Enabled, EmployeeIDs: append([]int64{}, panel.EmployeeIDs...)}, nil
}
func (m *memoryPanels) CreatePanel(_ context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, panel serviceapi.ManagedPanel, hash []byte) (serviceapi.ManagedPanel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if old, ok := m.receipts[c.CommandID]; ok {
		return old, nil
	}
	panel.Revision = 1
	panel.ViewKeyVersion = 1
	panel.ShareUrlIssued = true
	panel.ID = uuid.New()
	m.panels[panel.ID] = panel
	m.hashes[string(hash)] = panel.ID
	m.receipts[c.CommandID] = panel
	_ = p
	return panel, nil
}
func (m *memoryPanels) ListPanels(context.Context, serviceapi.Scope) ([]serviceapi.ManagedPanel, error) {
	return nil, nil
}
func (m *memoryPanels) GetPanel(_ context.Context, _ serviceapi.Scope, id uuid.UUID) (serviceapi.ManagedPanel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	panel, ok := m.panels[id]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return panel, nil
}
func (m *memoryPanels) PatchPanel(_ context.Context, _ serviceapi.Principal, c serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	panel, ok := m.panels[c.PanelID]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if panel.Revision != c.Revision {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "revision mismatch")
	}
	if c.Enabled != nil {
		panel.Enabled = *c.Enabled
	}
	panel.Revision++
	m.panels[c.PanelID] = panel
	return panel, nil
}
func (m *memoryPanels) RotateShareLink(_ context.Context, _ serviceapi.Principal, c serviceapi.PanelCommand, hash []byte, viewKey string) (serviceapi.ManagedPanel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	panel, ok := m.panels[c.PanelID]
	if !ok {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	for key, id := range m.hashes {
		if id == c.PanelID {
			delete(m.hashes, key)
		}
	}
	m.hashes[string(hash)] = c.PanelID
	panel.ViewKey = viewKey
	panel.ViewKeyVersion++
	panel.Revision++
	m.panels[c.PanelID] = panel
	return panel, nil
}

func TestViewerPanelOmitsSecretsAndRejectsOutsiders(t *testing.T) {
	store := &memoryPanels{}
	events := &fakeEvents{event: serviceapi.Event{ID: "outside", CreatedBy: 11, Type: "lead_added"}}
	gw := &fakeGateway{users: []serviceapi.User{{ID: 7, Name: "Alice"}, {ID: 9, Name: "Bob"}}}
	operator := &fakePolicy{kind: serviceapi.PrincipalKindOperator}
	s := New(store, operator, events, gw).WithShareOrigin("https://activity.example.invalid")
	auth := serviceapi.Auth{Token: "verified"}
	created, err := s.CreatePanel(context.Background(), serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), Name: "Shift", EmployeeIDs: []int64{7, 9}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil || created.ViewKey == "" || !strings.Contains(created.ShareUrl, created.ViewKey) {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	viewer := &fakePolicy{kind: serviceapi.PrincipalKindViewer, viewKeyVersion: 1, panelID: created.ID}
	vs := New(store, viewer, events, gw)
	panel, err := vs.ViewPanel(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(panel)
	if strings.Contains(string(raw), created.ViewKey) || strings.Contains(string(raw), "revision") || strings.Contains(string(raw), "share_url") {
		t.Fatalf("viewer leaked secrets: %s", raw)
	}
	if _, err := vs.ViewEvent(context.Background(), serviceapi.EventRequest{Auth: auth, EventID: "outside"}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("outside event=%v", err)
	}
	if _, err := vs.ViewEmployee(context.Background(), serviceapi.Query{Auth: auth, From: 100, To: 200, UserIDs: []int64{11}}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("outside employee=%v", err)
	}
	old := created.ViewKey
	rotated, err := s.RotateShareLink(context.Background(), serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), PanelID: created.ID})
	if err != nil || rotated.ViewKey == old {
		t.Fatalf("rotate=%+v err=%v", rotated, err)
	}
	if _, err := s.ResolveShare(context.Background(), serviceapi.ShareLookupRequest{ViewKey: old}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("old key=%v", err)
	}
	off := false
	if _, err := s.PatchPanel(context.Background(), serviceapi.PanelCommand{Auth: auth, PanelID: created.ID, Revision: rotated.Revision, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := vs.ViewPanel(context.Background(), auth); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("disabled=%v", err)
	}
}

func TestTwoPanelsShareHistoryWithoutDuplicatingEvents(t *testing.T) {
	store := &memoryPanels{}
	events := &fakeEvents{result: serviceapi.QueryResult{
		ReadVersion: serviceapi.PresentationReadVersion,
		Events:      []serviceapi.Event{{ID: "shared", CreatedAt: 150, CreatedBy: 7, Type: "lead_added"}},
		Summaries:   []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1}},
		Totals:      serviceapi.QueryTotals{UniqueEvents: 1},
	}}
	gw := &fakeGateway{users: []serviceapi.User{{ID: 7, Name: "Alice"}, {ID: 9, Name: "Bob"}}}
	operator := &fakePolicy{kind: serviceapi.PrincipalKindOperator}
	s := New(store, operator, events, gw)
	auth := serviceapi.Auth{Token: "verified"}
	first, err := s.CreatePanel(context.Background(), serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), Name: "A", EmployeeIDs: []int64{7, 9}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreatePanel(context.Background(), serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), Name: "B", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "10:00", To: "19:00"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.ViewKey == second.ViewKey {
		t.Fatalf("panels are not independent: %+v %+v", first, second)
	}
	query := serviceapi.Query{Auth: auth, From: 100, To: 200, Limit: 100}
	for _, panelID := range []uuid.UUID{first.ID, second.ID} {
		viewer := New(store, &fakePolicy{kind: serviceapi.PrincipalKindViewer, viewKeyVersion: 1, panelID: panelID}, events, gw)
		timeline, err := viewer.ViewTimeline(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if timeline.Data.Totals.UniqueEvents != 1 || len(timeline.Data.Events) != 1 || timeline.Data.Events[0].ID != "shared" {
			t.Fatalf("panel %s duplicated or lost the shared event: %+v", panelID, timeline.Data)
		}
	}
}
