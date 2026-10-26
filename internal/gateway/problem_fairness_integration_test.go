package gateway

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
	"time"
)

func TestPausedProblemAccountDoesNotBlockAnotherAndFinishesWithoutDemand(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	scopes := []serviceapi.Scope{{InstallationID: uuid.New(), IntegrationID: uuid.New()}, {InstallationID: uuid.New(), IntegrationID: uuid.New()}}
	for i, s := range scopes {
		if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'fixture','https://example.test/oauth')`, s.IntegrationID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,$3,'fixture.amocrm.ru','active')`, s.InstallationID, s.IntegrationID, 100+i); err != nil {
			t.Fatal(err)
		}
	}
	store := &ProblemStore{pool: pool, options: ProblemCollectionOptions{BatchSize: 500, BatchPause: time.Minute}}
	if err := store.Request(ctx, scopes[0]); err != nil {
		t.Fatal(err)
	}
	for p := 1; p <= 2; p++ {
		c, err := store.claim(ctx)
		if err != nil || c.page != p {
			t.Fatalf("claim %d: %+v %v", p, c, err)
		}
		leads := make([]amocrm.ProblemLead, 250)
		for j := range leads {
			leads[j] = amocrm.ProblemLead{ID: int64((p-1)*250 + j + 1), ResponsibleUserID: 7, StatusID: 11}
		}
		if err = store.apply(ctx, c, amocrm.ProblemPage{Leads: leads, HasNext: true}); err != nil {
			t.Fatal(err)
		}
	}
	var due time.Time
	var generation uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT next_poll_at,staging_generation FROM activity_problem_sources WHERE installation_id=$1`, scopes[0].InstallationID).Scan(&due, &generation); err != nil {
		t.Fatal(err)
	}
	if time.Until(due) < 55*time.Second {
		t.Fatal("missing pause")
	}
	// Restart with default options must retain the persisted pause and cursor.
	restarted := &ProblemStore{pool: pool}
	if err := restarted.Request(ctx, scopes[0]); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Request(ctx, scopes[1]); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"leads", "tasks"} {
		c, err := restarted.claim(ctx)
		if err != nil || c.scope.InstallationID != scopes[1].InstallationID || c.phase != phase {
			t.Fatalf("paused A blocked B: %+v %v", c, err)
		}
		if err = restarted.apply(ctx, c, amocrm.ProblemPage{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restarted.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("premature continuation or idle rescan: %v", err)
	}
	// Simulate the durable due time passing, with no viewers/subscription alive.
	if _, err := pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now(),requested_until=now()-interval '1 day' WHERE installation_id=$1`, scopes[0].InstallationID); err != nil {
		t.Fatal(err)
	}
	c, err := restarted.claim(ctx)
	if err != nil || c.page != 3 || c.generation != generation {
		t.Fatalf("resume lost page or depended on subscription: %+v %v", c, err)
	}
	if err = restarted.apply(ctx, c, amocrm.ProblemPage{}); err != nil {
		t.Fatal(err)
	}
	c, err = restarted.claim(ctx)
	if err != nil || c.phase != "tasks" {
		t.Fatalf("phase: %+v %v", c, err)
	}
	if err = restarted.apply(ctx, c, amocrm.ProblemPage{}); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Read(ctx, scopes[0], serviceapi.ProblemRequest{EmployeeIDs: []int64{7}})
	if err != nil || got.Collecting || len(got.Counts) != 1 || got.Counts[0].LeadsNoTasks != 500 {
		t.Fatalf("incomplete final snapshot %+v %v", got, err)
	}
	if _, err = restarted.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("autonomous rescan: %v", err)
	}
}
