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
	if err != nil || settings != Defaults() {
		t.Fatalf("conflict mutated settings=%+v err=%v", settings, err)
	}
}
