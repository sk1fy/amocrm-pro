package activity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"os"
	"strings"
	"testing"
)

type panelLifecycleChecker struct{}

func (panelLifecycleChecker) Check(context.Context, serviceapi.Scope, int64, bool) error { return nil }
func (panelLifecycleChecker) CheckDelegation(context.Context, serviceapi.Scope) error    { return nil }

func TestCreatePanelChangedEnabledConflicts(t *testing.T) {
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.ConnConfig.Database, "_test") {
		t.Fatal("requires a _test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("ACTIVITY_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	s := New(NewPostgres(pool), panelPolicy{principal: serviceapi.Principal{Scope: scope, Kind: serviceapi.PrincipalKindOperator}}, nil, &panelGateway{users: []serviceapi.User{{ID: 7, Name: "Synthetic"}}})
	enabled := false
	c := serviceapi.PanelCommand{Auth: serviceapi.Auth{Token: "ok"}, CommandID: uuid.NewString(), Name: "Synthetic", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}, Enabled: &enabled}
	first, err := s.CreatePanel(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	enabled = true
	replay, err := s.CreatePanel(ctx, c)
	if serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed enabled accepted: err=%v same_id=%t returned_enabled=%t", err, replay.ID == first.ID, replay.Enabled)
	}
}

func TestRotationRevokesIssuedViewerDelegation(t *testing.T) {
	ctx := context.Background()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(panelLifecycleChecker{}, key)
	if err != nil {
		t.Fatal(err)
	}
	core := corepolicy.ForCaller(policy, serviceapi.CoreService)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	operator, err := core.Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindOperator, Consumer: serviceapi.ActivityService, RequestID: "operator", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels)})
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryPanels{}
	s := New(store, corepolicy.ForCaller(policy, serviceapi.ActivityService), nil, &panelGateway{users: []serviceapi.User{{ID: 7, Name: "Synthetic"}}})
	panel, err := s.CreatePanel(ctx, serviceapi.PanelCommand{Auth: operator, CommandID: uuid.NewString(), Name: "Synthetic", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := core.Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: panel.ID, ViewKeyVersion: panel.ViewKeyVersion, Consumer: serviceapi.ActivityService, RequestID: "viewer", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ViewPanel(ctx, auth); err != nil {
		t.Fatal(err)
	}
	rotated, err := s.RotateShareLink(ctx, serviceapi.PanelCommand{Auth: operator, CommandID: uuid.NewString(), PanelID: panel.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveShare(ctx, serviceapi.ShareLookupRequest{ViewKey: panel.ViewKey}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("old key must be revoked: %v", err)
	}
	_, err = s.ViewPanel(ctx, auth)
	if serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("previous delegation after rotate: %v", err)
	}
	fresh, err := core.Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: panel.ID, ViewKeyVersion: rotated.ViewKeyVersion, Consumer: serviceapi.ActivityService, RequestID: "fresh-viewer", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ViewPanel(ctx, fresh); err != nil {
		t.Fatalf("new version denied: %v", err)
	}
}
