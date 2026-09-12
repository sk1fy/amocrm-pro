package crmevents

import (
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

func TestCursorBoundToViewerPanel(t *testing.T) {
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, Kind: serviceapi.PrincipalKindViewer, PanelID: uuid.New()}
	q := serviceapi.Query{From: 100, To: 200, UserIDs: []int64{7}, Limit: 1}
	q.Cursor = encodeCursor(q, p, serviceapi.Event{ID: "event", CreatedAt: 150})
	if _, err := decodeCursor(q, p); err != nil {
		t.Fatal(err)
	}
	p.PanelID = uuid.New()
	if _, err := decodeCursor(q, p); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("foreign panel cursor accepted: %v", err)
	}
	p.Kind = serviceapi.PrincipalKindUser
	q.Cursor = encodeCursor(q, p, serviceapi.Event{ID: "event", CreatedAt: 150})
	p.PanelID = uuid.Nil
	if _, err := decodeCursor(q, p); err != nil {
		t.Fatalf("widget cursor changed: %v", err)
	}
}
