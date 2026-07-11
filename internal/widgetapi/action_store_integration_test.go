package widgetapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func TestActionStoreIdempotentPingContract(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	principal := widgetPrincipal(t, pool, 42, 7)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := store.EnqueuePing(ctx, principal, "stable-ping-request")
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID == uuid.Nil || first.Status != jobs.StatusQueued || first.Replayed {
		t.Fatalf("first action result = %+v", first)
	}

	fresh := principal
	fresh.TokenID = uuid.NewString()
	replayed, err := store.EnqueuePing(ctx, fresh, "stable-ping-request")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.JobID != first.JobID || replayed.Status != first.Status || !replayed.Replayed {
		t.Fatalf("replayed action result = %+v, want job %s", replayed, first.JobID)
	}
	if _, err := store.EnqueuePing(ctx, principal, "stable-ping-request"); !errors.Is(err, widgetauth.ErrReplay) {
		t.Fatalf("same JWT replay error = %v, want ErrReplay", err)
	}

	conflictingActor := principal
	conflictingActor.TokenID = uuid.NewString()
	conflictingActor.UserID++
	if _, err := store.EnqueuePing(ctx, conflictingActor, "stable-ping-request"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("actor mismatch error = %v, want ErrIdempotencyConflict", err)
	}

	assertWidgetCounts(t, pool, 3, 1, 1)
}

func TestActionStoreConcurrentTokenAndIdempotencyFencing(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	principal := widgetPrincipal(t, pool, 55, 9)

	t.Run("one disposable token", func(t *testing.T) {
		const callers = 16
		var wait sync.WaitGroup
		wait.Add(callers)
		results := make(chan error, callers)
		for index := 0; index < callers; index++ {
			go func(index int) {
				defer wait.Done()
				_, err := store.EnqueuePing(context.Background(), principal, fmt.Sprintf("token-race-%d", index))
				results <- err
			}(index)
		}
		wait.Wait()
		close(results)
		successes, replays := 0, 0
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, widgetauth.ErrReplay):
				replays++
			default:
				t.Fatalf("unexpected concurrent token error: %v", err)
			}
		}
		if successes != 1 || replays != callers-1 {
			t.Fatalf("success/replay = %d/%d", successes, replays)
		}
	})

	testkit.Reset(t, pool)
	principal = widgetPrincipal(t, pool, 55, 9)
	t.Run("fresh tokens share one idempotency key", func(t *testing.T) {
		const callers = 16
		var wait sync.WaitGroup
		wait.Add(callers)
		results := make(chan ActionResult, callers)
		errorsSeen := make(chan error, callers)
		for index := 0; index < callers; index++ {
			go func() {
				defer wait.Done()
				fresh := principal
				fresh.TokenID = uuid.NewString()
				result, err := store.EnqueuePing(context.Background(), fresh, "shared-idempotency-key")
				results <- result
				errorsSeen <- err
			}()
		}
		wait.Wait()
		close(results)
		close(errorsSeen)
		for err := range errorsSeen {
			if err != nil {
				t.Fatalf("concurrent idempotency error: %v", err)
			}
		}
		var jobID uuid.UUID
		for result := range results {
			if jobID == uuid.Nil {
				jobID = result.JobID
			}
			if result.JobID != jobID {
				t.Fatalf("job id = %s, want %s", result.JobID, jobID)
			}
		}
		assertWidgetCounts(t, pool, callers, 1, 1)
	})
}

func TestActionStoreRollsBackTokenKeyAndJobTogether(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	principal := widgetPrincipal(t, pool, 77, 11)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_widget_ping_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic widget job failure'; END;
		$$;
		CREATE TRIGGER fail_widget_ping_insert
		BEFORE INSERT ON jobs FOR EACH ROW
		WHEN (NEW.type = 'widget.ping') EXECUTE FUNCTION fail_widget_ping_insert()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS fail_widget_ping_insert ON jobs;
			DROP FUNCTION IF EXISTS fail_widget_ping_insert()`)
	})
	if _, err := store.EnqueuePing(ctx, principal, "rollback-request"); err == nil {
		t.Fatal("EnqueuePing() unexpectedly succeeded")
	}
	assertWidgetCounts(t, pool, 0, 0, 0)
	if _, err := pool.Exec(ctx, `
		DROP TRIGGER fail_widget_ping_insert ON jobs;
		DROP FUNCTION fail_widget_ping_insert()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueuePing(ctx, principal, "rollback-request"); err != nil {
		t.Fatalf("retry after rollback error = %v", err)
	}
	assertWidgetCounts(t, pool, 1, 1, 1)
}

func TestActionStoreRejectsInactiveTenantBeforeConsumption(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	principal := widgetPrincipal(t, pool, 88, 13)
	if _, err := pool.Exec(context.Background(), `
		UPDATE installations SET status='disabled' WHERE id=$1`, principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueuePing(context.Background(), principal, "inactive-request"); !errors.Is(err, ErrInactiveTenant) {
		t.Fatalf("EnqueuePing() error = %v, want ErrInactiveTenant", err)
	}
	assertWidgetCounts(t, pool, 0, 0, 0)
}

func TestActionStoreRejectsInactiveIntegrationBeforeConsumption(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	principal := widgetPrincipal(t, pool, 89, 15)
	if _, err := pool.Exec(context.Background(), `
		UPDATE integrations SET status='disabled' WHERE id=$1`, principal.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueuePing(context.Background(), principal, "inactive-integration"); !errors.Is(err, ErrInactiveTenant) {
		t.Fatalf("EnqueuePing() error = %v, want ErrInactiveTenant", err)
	}
	assertWidgetCounts(t, pool, 0, 0, 0)
}

func TestActionStoreScopesKeysByInstallationAndReclaimsExpiredRows(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	store := NewActionStore(pool, jobs.NewStore(pool))
	firstTenant := widgetPrincipal(t, pool, 91, 29)
	secondTenant := widgetPrincipal(t, pool, 92, 31)

	first, err := store.EnqueuePing(context.Background(), firstTenant, "shared-across-tenants")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnqueuePing(context.Background(), secondTenant, "shared-across-tenants")
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID == second.JobID {
		t.Fatalf("different tenants share job %s", first.JobID)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE idempotency_keys
		SET created_at=now()-interval '2 hours', expires_at=now()-interval '1 hour'
		WHERE installation_id=$1`, firstTenant.InstallationID); err != nil {
		t.Fatal(err)
	}
	fresh := firstTenant
	fresh.TokenID = uuid.NewString()
	reclaimed, err := store.EnqueuePing(context.Background(), fresh, "shared-across-tenants")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.JobID == first.JobID || reclaimed.Replayed {
		t.Fatalf("expired key result = %+v, old job = %s", reclaimed, first.JobID)
	}
	assertWidgetCounts(t, pool, 3, 2, 3)
}

func widgetPrincipal(t *testing.T, pool *pgxpool.Pool, accountID, userID int64) widgetauth.Principal {
	t.Helper()
	integrationID := uuid.New()
	installationID := uuid.New()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri
		) VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		integrationID, "widget-"+integrationID.String(), uuid.NewString(), []byte{1},
		"https://integration.example.test/widget/callback",
	).Scan(&integrationID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO installations (id, integration_id, account_id, account_domain, status)
		VALUES ($1, $2, $3, $4, 'active') RETURNING id`,
		installationID, integrationID, accountID, "tenant.amocrm.ru",
	).Scan(&installationID); err != nil {
		t.Fatal(err)
	}
	return widgetauth.Principal{
		IntegrationID:    integrationID,
		InstallationID:   installationID,
		AccountID:        accountID,
		UserID:           userID,
		ClientUUID:       "client-" + integrationID.String(),
		Issuer:           "https://tenant.amocrm.ru",
		TokenID:          uuid.NewString(),
		TokenRetainUntil: time.Now().UTC().Add(time.Hour),
	}
}

func assertWidgetCounts(t *testing.T, pool *pgxpool.Pool, tokens, keys, queuedJobs int) {
	t.Helper()
	var gotTokens, gotKeys, gotJobs int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM used_widget_tokens),
			(SELECT count(*) FROM idempotency_keys),
			(SELECT count(*) FROM jobs WHERE type='widget.ping')`,
	).Scan(&gotTokens, &gotKeys, &gotJobs); err != nil {
		t.Fatal(err)
	}
	if gotTokens != tokens || gotKeys != keys || gotJobs != queuedJobs {
		t.Fatalf("token/key/job counts = %d/%d/%d, want %d/%d/%d",
			gotTokens, gotKeys, gotJobs, tokens, keys, queuedJobs)
	}
}
