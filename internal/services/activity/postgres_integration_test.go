package activity

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"os"
	"strings"
	"testing"
)

func TestPostgresSettingsReceiptSurvivesResponseLoss(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE settings,command_receipts`); err != nil {
		t.Fatal(err)
	}
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7}
	s := NewPostgres(pool)
	command := serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: Defaults()}
	first, err := s.Configure(ctx, p, command)
	if err != nil {
		t.Fatal(err)
	}
	// A new adapter represents process restart after a successful commit whose
	// transport response was lost. Replaying cannot apply a second mutation.
	s = NewPostgres(pool)
	second, err := s.Configure(ctx, p, command)
	if err != nil || first != second {
		t.Fatalf("replay=%+v err=%v", second, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM command_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v", count, err)
	}
	command.Settings.RetentionDays++
	if _, err := s.Configure(ctx, p, command); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed payload=%v", err)
	}
	other := p
	other.ActorID++
	if _, err := s.Operation(ctx, other, first.ID); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign actor=%v", err)
	}
	other = p
	other.IntegrationID = uuid.New()
	if _, err := s.Operation(ctx, other, first.ID); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign integration=%v", err)
	}
	settings, err := s.Settings(ctx, p.Scope)
	if err != nil || settings.InitialDays != Defaults().InitialDays || settings.RetentionDays != Defaults().RetentionDays {
		t.Fatalf("conflict mutated settings=%+v err=%v", settings, err)
	}
	// A mismatched owner must fail, not create a successful receipt for an
	// upsert suppressed by its tenant predicate. Its transaction rolls back.
	foreignCommand := serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 8}}
	if _, err := s.Configure(ctx, other, foreignCommand); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("foreign settings configure=%v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM command_receipts WHERE command_id=$1`, foreignCommand.CommandID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied configure left a receipt: %d %v", count, err)
	}
	// The same command identity is still usable by its legitimate scope.
	if _, err := s.Configure(ctx, p, foreignCommand); err != nil {
		t.Fatalf("denied attempt poisoned command identity: %v", err)
	}
	settings, err = s.Settings(ctx, p.Scope)
	if err != nil || settings.InitialDays != foreignCommand.Settings.InitialDays || settings.RetentionDays != foreignCommand.Settings.RetentionDays || settings.UpdatedAt == 0 {
		t.Fatalf("owner update=%+v %v", settings, err)
	}
}

func TestPostgresSettingsCASExpectedUpdatedAt(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE settings,command_receipts`); err != nil {
		t.Fatal(err)
	}
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7}
	s := NewPostgres(pool)
	zero := int64(0)
	first := serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 2, RetentionDays: 7}, ExpectedUpdatedAt: &zero}
	if _, err := s.Configure(ctx, p, first); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Settings(ctx, p.Scope)
	if err != nil || saved.UpdatedAt == 0 {
		t.Fatalf("updated_at after save=%+v %v", saved, err)
	}
	stale := saved.UpdatedAt - 1
	if _, err := s.Configure(ctx, p, serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 8}, ExpectedUpdatedAt: &stale}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("stale CAS=%v", err)
	}
	unchanged, err := s.Settings(ctx, p.Scope)
	if err != nil || unchanged.InitialDays != 2 || unchanged.RetentionDays != 7 || unchanged.UpdatedAt != saved.UpdatedAt {
		t.Fatalf("mismatch applied=%+v %v", unchanged, err)
	}
	expected := unchanged.UpdatedAt
	if _, err := s.Configure(ctx, p, serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 9}, ExpectedUpdatedAt: &expected}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Configure(ctx, p, serviceapi.SettingsCommand{CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 1, RetentionDays: 4}}); err != nil {
		t.Fatalf("widget-like last write=%v", err)
	}
	got, err := s.Settings(ctx, p.Scope)
	if err != nil || got.InitialDays != 1 || got.RetentionDays != 4 {
		t.Fatalf("widget-like settings=%+v %v", got, err)
	}
}
