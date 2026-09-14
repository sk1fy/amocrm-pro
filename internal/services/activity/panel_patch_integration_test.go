package activity

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestPostgresPatchPanelDurableReplayAndIsolation(t *testing.T) {
	ctx := context.Background()
	pool := panelPatchDatabase(t, ctx)
	if _, err := pool.Exec(ctx, `TRUNCATE panels,panel_commands`); err != nil {
		t.Fatal(err)
	}

	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	principal := serviceapi.Principal{Scope: scope, ActorID: 42, Kind: serviceapi.PrincipalKindOperator}
	gateway := &panelGateway{users: []serviceapi.User{{ID: 7, Name: "Alice"}}}
	service := New(NewPostgres(pool), panelPolicy{principal: principal}, nil, gateway)
	auth := serviceapi.Auth{Token: "ok"}
	created, err := service.CreatePanel(ctx, serviceapi.PanelCommand{
		Auth: auth, CommandID: uuid.NewString(), Name: "Original", EmployeeIDs: []int64{7},
		DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"},
	})
	if err != nil {
		t.Fatal(err)
	}

	command := serviceapi.PanelCommand{
		Auth: auth, CommandID: uuid.NewString(), PanelID: created.ID, Revision: created.Revision,
		Name: "First result", HasName: true,
	}
	first, err := service.PatchPanel(ctx, command)
	if err != nil || first.Name != command.Name || first.Revision != created.Revision+1 {
		t.Fatalf("first patch=%+v err=%v", first, err)
	}
	var kind, snapshot string
	if err := pool.QueryRow(ctx, `SELECT kind,result::text FROM panel_commands WHERE command_id=$1`, command.CommandID).Scan(&kind, &snapshot); err != nil {
		t.Fatal(err)
	}
	if kind != "patch" || strings.Contains(snapshot, `"view_key":`) || strings.Contains(snapshot, `"share_url":`) || strings.Contains(snapshot, "token") {
		t.Fatalf("unsafe patch snapshot: kind=%s result=%s", kind, snapshot)
	}

	disabled := false
	changed, err := service.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, PanelID: created.ID, Revision: first.Revision, Enabled: &disabled})
	if err != nil || changed.Revision != first.Revision+1 {
		t.Fatalf("legacy CAS patch=%+v err=%v", changed, err)
	}
	restarted := New(NewPostgres(pool), panelPolicy{principal: principal}, nil, gateway)
	replay, err := restarted.PatchPanel(ctx, command)
	if err != nil || replay.ID != first.ID || replay.Name != first.Name || replay.Enabled != first.Enabled || replay.Revision != first.Revision || !replay.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("replay did not return first snapshot: first=%+v replay=%+v err=%v", first, replay, err)
	}

	different := command
	different.Name = "Different payload"
	if _, err := restarted.PatchPanel(ctx, different); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("different payload=%v", err)
	}
	otherActor := New(NewPostgres(pool), panelPolicy{principal: serviceapi.Principal{Scope: scope, ActorID: 43, Kind: serviceapi.PrincipalKindOperator}}, nil, gateway)
	if _, err := otherActor.PatchPanel(ctx, command); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("different actor=%v", err)
	}
	otherScope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	foreign := New(NewPostgres(pool), panelPolicy{principal: serviceapi.Principal{Scope: otherScope, ActorID: 42, Kind: serviceapi.PrincipalKindOperator}}, nil, gateway)
	if _, err := foreign.PatchPanel(ctx, command); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign scope learned receipt=%v", err)
	}

	corrupt := serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), PanelID: created.ID, Revision: changed.Revision, Name: "Must not run", HasName: true}
	hash := panelPatchRequestHash(principal, corrupt)
	if _, err := pool.Exec(ctx, `INSERT INTO panel_commands(command_id,installation_id,integration_id,panel_id,kind,request_hash,result) VALUES($1,$2,$3,$4,'patch',$5,NULL)`, corrupt.CommandID, scope.InstallationID, scope.IntegrationID, corrupt.PanelID, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.PatchPanel(ctx, corrupt); serviceapi.ErrorCode(err) != serviceapi.Internal {
		t.Fatalf("corrupt receipt=%v", err)
	}
	current, err := restarted.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: created.ID})
	if err != nil || current.Name != changed.Name || current.Revision != changed.Revision {
		t.Fatalf("corrupt receipt repeated mutation: panel=%+v err=%v", current, err)
	}

	concurrent := serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), PanelID: created.ID, Revision: current.Revision, Name: "Concurrent result", HasName: true}
	const callers = 12
	start := make(chan struct{})
	results := make(chan serviceapi.ManagedPanel, callers)
	errors := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			panel, err := restarted.PatchPanel(ctx, concurrent)
			results <- panel
			errors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent replay=%v", err)
		}
	}
	for panel := range results {
		if panel.Name != concurrent.Name || panel.Revision != current.Revision+1 {
			t.Fatalf("concurrent result=%+v", panel)
		}
	}
	current, err = restarted.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: created.ID})
	if err != nil || current.Revision != concurrent.Revision+1 {
		t.Fatalf("concurrent command mutated more than once: panel=%+v err=%v", current, err)
	}

	downSQL, err := os.ReadFile("../../../migrations/activity/000004_panel_patch_results.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(downSQL)); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("down migration discarded durable patch receipts")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresPatchPanelRollsBackWhenResultCannotBeStored(t *testing.T) {
	ctx := context.Background()
	pool := panelPatchDatabase(t, ctx)
	if _, err := pool.Exec(ctx, `TRUNCATE panels,panel_commands`); err != nil {
		t.Fatal(err)
	}
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	principal := serviceapi.Principal{Scope: scope, ActorID: 42, Kind: serviceapi.PrincipalKindOperator}
	service := New(NewPostgres(pool), panelPolicy{principal: principal}, nil, &panelGateway{users: []serviceapi.User{{ID: 7, Name: "Alice"}}})
	auth := serviceapi.Auth{Token: "ok"}
	created, err := service.CreatePanel(ctx, serviceapi.PanelCommand{
		Auth: auth, CommandID: uuid.NewString(), Name: "Before", EmployeeIDs: []int64{7},
		DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE panel_commands ADD CONSTRAINT panel_patch_result_fault CHECK (result IS NULL) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE panel_commands DROP CONSTRAINT IF EXISTS panel_patch_result_fault`)
	}()

	commandID := uuid.NewString()
	_, err = service.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: commandID, PanelID: created.ID, Revision: created.Revision, Name: "After", HasName: true})
	if err == nil {
		t.Fatal("patch unexpectedly succeeded while result writes were rejected")
	}
	current, getErr := service.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: created.ID})
	if getErr != nil || current.Name != created.Name || current.Revision != created.Revision {
		t.Fatalf("panel mutation was not rolled back: panel=%+v err=%v patch_err=%v", current, getErr, err)
	}
	var receipts int
	if scanErr := pool.QueryRow(ctx, `SELECT count(*) FROM panel_commands WHERE command_id=$1`, commandID).Scan(&receipts); scanErr != nil || receipts != 0 {
		t.Fatalf("failed transaction left receipt: count=%d err=%v", receipts, scanErr)
	}
}

func panelPatchDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ACTIVITY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ACTIVITY_TEST_DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("Activity integration test requires a _test database")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.New(pool, "../../../migrations/activity").Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}
