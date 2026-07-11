package maintenance

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestCleanupStrictExpirySafetyMarginAndJobRetention(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integrationID, installationID := cleanupTenant(t, pool)
	ctx := context.Background()

	insertToken(t, pool, integrationID, "expired", "2 hours ago")
	insertToken(t, pool, integrationID, "inside-margin", "30 minutes ago")
	insertToken(t, pool, integrationID, "future", "1 hour")
	jobID := insertJob(t, pool, installationID)
	insertIdempotency(t, pool, installationID, jobID, "completed", "2 hours ago", "completed")
	insertIdempotency(t, pool, installationID, uuid.Nil, "failed", "2 hours ago", "failed")
	insertIdempotency(t, pool, installationID, uuid.Nil, "processing", "2 hours ago", "processing")
	insertIdempotency(t, pool, installationID, uuid.Nil, "inside", "30 minutes ago", "processing")

	result, err := NewStore(pool).Cleanup(ctx, Policy{
		SafetyMargin: time.Hour, BatchSize: 100, MaxBatches: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.LockAcquired || result.WidgetTokens != 1 || result.IdempotencyKeys != 3 {
		t.Fatalf("cleanup result = %+v", result)
	}
	var tokens, keys, jobs int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM used_widget_tokens),
		(SELECT count(*) FROM idempotency_keys),
		(SELECT count(*) FROM jobs WHERE id=$1)`, jobID).Scan(&tokens, &keys, &jobs); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 || keys != 1 || jobs != 1 {
		t.Fatalf("remaining token/key/job counts = %d/%d/%d", tokens, keys, jobs)
	}
}

func TestCleanupHonorsBatchAndMaximumBatchBounds(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integrationID, installationID := cleanupTenant(t, pool)
	for index := 0; index < 5; index++ {
		insertToken(t, pool, integrationID, uuid.NewString(), "1 hour ago")
		insertIdempotency(t, pool, installationID, uuid.Nil, uuid.NewString(), "1 hour ago", "processing")
	}
	store := NewStore(pool)
	first, err := store.Cleanup(context.Background(), Policy{BatchSize: 2, MaxBatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.WidgetTokens != 2 || first.IdempotencyKeys != 2 {
		t.Fatalf("first bounded result = %+v", first)
	}
	second, err := store.Cleanup(context.Background(), Policy{BatchSize: 2, MaxBatches: 2})
	if err != nil {
		t.Fatal(err)
	}
	if second.WidgetTokens != 3 || second.IdempotencyKeys != 3 {
		t.Fatalf("second bounded result = %+v", second)
	}
}

func TestCleanupAdvisoryLockSkipsConcurrentWorker(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integrationID, _ := cleanupTenant(t, pool)
	insertToken(t, pool, integrationID, "slow-delete", "1 hour ago")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION delay_cleanup_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_sleep(0.3); RETURN OLD; END;
		$$;
		CREATE TRIGGER delay_cleanup_delete BEFORE DELETE ON used_widget_tokens
		FOR EACH ROW EXECUTE FUNCTION delay_cleanup_delete()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS delay_cleanup_delete ON used_widget_tokens;
			DROP FUNCTION IF EXISTS delay_cleanup_delete()`)
	})

	store := NewStore(pool)
	var wait sync.WaitGroup
	wait.Add(1)
	firstDone := make(chan error, 1)
	go func() {
		wait.Done()
		_, err := store.Cleanup(ctx, Policy{BatchSize: 1, MaxBatches: 1})
		firstDone <- err
	}()
	wait.Wait()
	time.Sleep(50 * time.Millisecond)
	second, err := store.Cleanup(ctx, Policy{BatchSize: 1, MaxBatches: 1})
	if err != nil {
		t.Fatal(err)
	}
	if second.LockAcquired {
		t.Fatalf("concurrent cleanup acquired lock: %+v", second)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func cleanupTenant(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	integrationID, installationID := uuid.New(), uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO integrations (id, code, client_id, client_secret_ciphertext, redirect_uri)
		VALUES ($1,$2,$3,decode('00','hex'),'https://example.test/oauth')`,
		integrationID, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installations (id,integration_id,account_id,account_domain,status)
		VALUES ($1,$2,42,'tenant.amocrm.ru','active')`,
		installationID, integrationID); err != nil {
		t.Fatal(err)
	}
	return integrationID, installationID
}

func insertToken(t *testing.T, pool *pgxpool.Pool, integrationID uuid.UUID, jti, expiry string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO used_widget_tokens (
			integration_id,jti,issuer,account_id,user_id,expires_at,created_at
		) VALUES (
			$1,$2,'https://tenant.amocrm.ru',42,7,now()+$3::interval,
			LEAST(now(), now()+$3::interval-interval '1 hour')
		)`,
		integrationID, jti, expiry); err != nil {
		t.Fatal(err)
	}
}

func insertJob(t *testing.T, pool *pgxpool.Pool, installationID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO jobs (installation_id,type,payload) VALUES ($1,'cleanup.fixture','{}') RETURNING id`,
		installationID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertIdempotency(
	t *testing.T, pool *pgxpool.Pool, installationID, jobID uuid.UUID,
	key, expiry, status string,
) {
	t.Helper()
	keyHash := sha256.Sum256([]byte("key:" + key))
	requestHash := sha256.Sum256([]byte("request:" + key))
	var nullableJob any
	var responseStatus any
	var responseBody any
	if jobID != uuid.Nil {
		nullableJob, responseStatus, responseBody = jobID, 202, `{"job_id":"`+jobID.String()+`"}`
	}
	if status == "failed" {
		responseStatus, responseBody = 409, `{"error":"failed"}`
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO idempotency_keys (
			installation_id,scope,key_hash,request_hash,status,job_id,
			response_status,response_body,expires_at,created_at,updated_at
		) VALUES (
			$1,'cleanup:v1',$2,$3,$4,$5,$6,$7,now()+$8::interval,
			LEAST(now(), now()+$8::interval-interval '1 hour'),
			LEAST(now(), now()+$8::interval-interval '1 hour')
		)`,
		installationID, keyHash[:], requestHash[:], status, nullableJob,
		responseStatus, responseBody, expiry); err != nil {
		t.Fatal(err)
	}
}
