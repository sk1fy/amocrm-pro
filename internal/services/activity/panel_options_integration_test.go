package activity

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPanelOptionsPersistCASReplayAndScope(t *testing.T) {
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("requires isolated _test database")
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
	store := NewPostgres(pool)
	principal := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, Kind: serviceapi.PrincipalKindOperator}
	s := New(store, panelPolicy{principal: principal}, nil, &panelGateway{users: []serviceapi.User{{ID: 7}, {ID: 9}}})
	auth := serviceapi.Auth{Token: "ok"}
	options := serviceapi.PanelOptions{PipelineIDs: []int64{123}, Operators: []serviceapi.OperatorSettings{{EmployeeID: 7, Enabled: true, Plan: 100, Thresholds: serviceapi.CallThresholds{DialingSeconds: 10, ConversationSeconds: 30, ExceedingSeconds: 180}}}}
	create := serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), Name: "Options", EmployeeIDs: []int64{7, 9}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}, Options: options, HasOptions: true}
	p, err := s.CreatePanel(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Options, options) {
		t.Fatalf("options not persisted: %+v", p.Options)
	}
	// New command with same id and different options must not replay the old creation.
	create.Options.Operators = append([]serviceapi.OperatorSettings{}, options.Operators...)
	create.Options.Operators[0].Plan = 101
	if _, err = s.CreatePanel(ctx, create); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed options replay accepted: %v", err)
	}
	patch := serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), PanelID: p.ID, Revision: p.Revision, Options: create.Options, HasOptions: true}
	updated, err := s.PatchPanel(ctx, patch)
	if err != nil || updated.Options.Operators[0].Plan != 101 {
		t.Fatalf("patch: %+v %v", updated, err)
	}
	replay, err := s.PatchPanel(ctx, patch)
	if err != nil || replay.Revision != updated.Revision || replay.Options.Operators[0].Plan != 101 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	patch.CommandID = uuid.NewString()
	if _, err = s.PatchPanel(ctx, patch); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	// Removing an assigned operator without changing the options is invalid.
	if _, err = s.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, PanelID: p.ID, Revision: updated.Revision, HasEmployees: true, EmployeeIDs: []int64{9}}); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("orphan operator accepted: %v", err)
	}
	foreign := principal.Scope
	foreign.InstallationID = uuid.New()
	if _, err = store.GetPanel(ctx, foreign, p.ID); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("cross installation read: %v", err)
	}
	// Independent panels retain their own settings.
	create.CommandID = uuid.NewString()
	create.Name = "Other"
	create.Options = serviceapi.PanelOptions{}
	other, err := s.CreatePanel(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Options.Operators) != 0 {
		t.Fatal("panel options leaked")
	}
	viewer := principal
	viewer.PanelID = p.ID
	viewer.ViewKeyVersion = p.ViewKeyVersion
	viewer.Kind = serviceapi.PrincipalKindViewer
	got, err := New(store, panelPolicy{principal: viewer}, nil, &panelGateway{users: []serviceapi.User{{ID: 7}, {ID: 9}}}).ViewPanel(ctx, auth)
	if err != nil || got.Options.Operators[0].Plan != 101 {
		t.Fatalf("viewer projection %+v %v", got, err)
	}
}
