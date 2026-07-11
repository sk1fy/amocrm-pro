package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
)

func TestCleanupWebhookPayloadRetentionPreservesDurableHistoryAndReplaySuppression(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	_, installationID := cleanupTenant(t, pool)
	ctx := context.Background()
	originKey := sha256.Sum256([]byte("retained-origin"))
	otherKey := sha256.Sum256([]byte("retained-pending"))
	desiredHash := sha256.Sum256([]byte("desired-state"))

	var expiredDeliveryID, retainedDeliveryID, expiredEventID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (
			installation_id,content_type,raw_body,body_sha256,parse_status,
			received_at,parsed_at,updated_at
		) VALUES ($1,'application/x-www-form-urlencoded',decode('01','hex'),$2,'parsed',
			now()-interval '4 hours',now()-interval '3 hours',now()-interval '3 hours')
		RETURNING id`, installationID, originKey[:]).Scan(&expiredDeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_event_tombstones (
			installation_id,deduplication_key,first_seen_at,last_seen_at
		) VALUES ($1,$2,now()-interval '4 hours',now()-interval '3 hours')`,
		installationID, originKey[:]); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO inbox_events (
			delivery_id,installation_id,entity_type,event_type,entity_id,payload,
			deduplication_key,status,processed_at,created_at,updated_at
		) VALUES ($1,$2,'leads','status',42,'{"pipeline_id":1,"status_id":2}',
			$3,'processed',now()-interval '3 hours',now()-interval '4 hours',now()-interval '3 hours')
		RETURNING id`, expiredDeliveryID, installationID, originKey[:]).Scan(&expiredEventID); err != nil {
		t.Fatal(err)
	}

	var ruleID, jobID, runID, effectID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO lead_status_workflow_rules (
			installation_id,source_pipeline_id,source_status_id,target_pipeline_id,target_status_id
		) VALUES ($1,1,2,1,3) RETURNING id`, installationID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO jobs (installation_id,type,status,payload,created_at,updated_at,finished_at)
		VALUES ($1,'workflow.lead.status_transition','completed','{}',
			now()-interval '4 hours',now()-interval '2 hours',now()-interval '2 hours')
		RETURNING id`, installationID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (
			installation_id,workflow_type,origin_deduplication_key,origin_event_id,
			rule_id,job_id,status,created_at,finished_at
		) VALUES ($1,'lead.status_transition',$2,$3,$4,$5,'completed',
			now()-interval '4 hours',now()-interval '2 hours') RETURNING id`,
		installationID, originKey[:], expiredEventID, ruleID, jobID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO outbound_effects (
			installation_id,workflow_run_id,correlation_job_id,effect_type,
			resource_type,resource_id,desired_state,desired_hash,state,attempted_at,
			observed_at,correlation_expires_at,correlated_event_deduplication_key,
			created_at,updated_at
		) VALUES ($1,$2,$3,'lead.set_status','lead','42','{"pipeline_id":1,"status_id":3}',
			$4,'observed',now()-interval '3 hours',now()-interval '2 hours',
			now()-interval '1 hour',$5,now()-interval '3 hours',now()-interval '2 hours')
		RETURNING id`, installationID, runID, jobID, desiredHash[:], originKey[:]).Scan(&effectID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE inbox_events SET correlated_effect_id=$2 WHERE id=$1`, expiredEventID, effectID); err != nil {
		t.Fatal(err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (
			installation_id,content_type,raw_body,body_sha256,parse_status,
			received_at,parsed_at,updated_at
		) VALUES ($1,'application/x-www-form-urlencoded',decode('02','hex'),$2,'parsed',
			now()-interval '4 hours',now()-interval '3 hours',now()-interval '3 hours')
		RETURNING id`, installationID, otherKey[:]).Scan(&retainedDeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO inbox_events (
			delivery_id,installation_id,entity_type,event_type,payload,deduplication_key,
			status,created_at,updated_at
		) VALUES ($1,$2,'leads','update','{}',$3,'pending',
			now()-interval '4 hours',now()-interval '3 hours')`,
		retainedDeliveryID, installationID, otherKey[:]); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"invalid", "parsed"} {
		age := "3 hours"
		if status == "parsed" {
			age = "30 minutes"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO webhook_deliveries (
				installation_id,content_type,raw_body,body_sha256,parse_status,
				received_at,parsed_at,updated_at
			) VALUES ($1,'application/x-www-form-urlencoded',decode('03','hex'),$2,$3,
				now()-$4::interval,now()-$4::interval,now()-$4::interval)`,
			installationID, desiredHash[:], status, age); err != nil {
			t.Fatal(err)
		}
	}

	time.Sleep(5 * time.Millisecond)
	policy := testPolicy(100, 2)
	policy.WebhookInboxRetention = time.Millisecond
	result, err := NewStore(pool).Cleanup(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.InboxEvents != 1 || result.WebhookDeliveries != 2 {
		t.Fatalf("webhook cleanup result = %+v", result)
	}
	var tombstones, runs, effects, jobs, originLinks, expiredDeliveries, retainedDeliveries int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM webhook_event_tombstones WHERE installation_id=$1 AND deduplication_key=$2),
		(SELECT count(*) FROM workflow_runs WHERE id=$3),
		(SELECT count(*) FROM outbound_effects WHERE id=$4),
		(SELECT count(*) FROM jobs WHERE id=$5),
		(SELECT count(*) FROM workflow_runs WHERE id=$3 AND origin_event_id IS NOT NULL),
		(SELECT count(*) FROM webhook_deliveries WHERE id=$6),
		(SELECT count(*) FROM webhook_deliveries WHERE id=$7)`,
		installationID, originKey[:], runID, effectID, jobID, expiredDeliveryID, retainedDeliveryID,
	).Scan(&tombstones, &runs, &effects, &jobs, &originLinks, &expiredDeliveries, &retainedDeliveries); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 || runs != 1 || effects != 1 || jobs != 1 || originLinks != 0 ||
		expiredDeliveries != 0 || retainedDeliveries != 1 {
		t.Fatalf("retained history=%d/%d/%d/%d origin_links=%d deliveries=%d/%d",
			tombstones, runs, effects, jobs, originLinks, expiredDeliveries, retainedDeliveries)
	}

	var replayDeliveryID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (
			installation_id,content_type,raw_body,body_sha256
		) VALUES ($1,'application/x-www-form-urlencoded',decode('04','hex'),$2)
		RETURNING id`, installationID, originKey[:]).Scan(&replayDeliveryID); err != nil {
		t.Fatal(err)
	}
	inserted, err := webhook.NewStore(pool).SaveParsedEvents(ctx, webhook.Delivery{
		ID: replayDeliveryID, InstallationID: installationID,
	}, []webhook.Event{{
		EntityType: "leads", EventType: "status", Payload: json.RawMessage(`{"pipeline_id":1,"status_id":2}`),
		DeduplicationKey: originKey[:],
	}})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 {
		t.Fatalf("replayed event insert count = %d", inserted)
	}
	var replayEvents, replayJobs int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM inbox_events WHERE installation_id=$1 AND deduplication_key=$2),
		(SELECT count(*) FROM jobs WHERE type='webhook.process_event' AND installation_id=$1)`,
		installationID, originKey[:]).Scan(&replayEvents, &replayJobs); err != nil {
		t.Fatal(err)
	}
	if replayEvents != 0 || replayJobs != 0 {
		t.Fatalf("replay events/jobs = %d/%d", replayEvents, replayJobs)
	}
}

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
		SafetyMargin: time.Hour, WebhookInboxRetention: time.Hour,
		WebhookDeliveryRetention: time.Hour, BatchSize: 100, MaxBatches: 2,
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

func TestCleanupWebhookInboxDeletesOnlyTerminalStatuses(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	_, installationID := cleanupTenant(t, pool)
	ctx := context.Background()
	statuses := []string{"processed", "failed", "dead", "ignored", "pending", "processing"}
	for index, status := range statuses {
		key := sha256.Sum256([]byte(status))
		bodyHash := sha256.Sum256([]byte("body:" + status))
		var deliveryID uuid.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO webhook_deliveries (
				installation_id,content_type,raw_body,body_sha256,parse_status,
				received_at,parsed_at,updated_at
			) VALUES ($1,'application/x-www-form-urlencoded',$2,$3,'parsed',
				now()-interval '4 hours',now()-interval '3 hours',now()-interval '3 hours')
			RETURNING id`, installationID, []byte{byte(index)}, bodyHash[:]).Scan(&deliveryID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO inbox_events (
				delivery_id,installation_id,entity_type,event_type,payload,
				deduplication_key,status,created_at,updated_at
			) VALUES ($1,$2,'leads','update','{}',$3,$4,
				now()-interval '4 hours',now()-interval '3 hours')`,
			deliveryID, installationID, key[:], status); err != nil {
			t.Fatal(err)
		}
	}
	policy := testPolicy(100, 1)
	policy.WebhookDeliveryRetention = 24 * time.Hour
	result, err := NewStore(pool).Cleanup(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.InboxEvents != 4 || result.WebhookDeliveries != 0 {
		t.Fatalf("terminal status cleanup = %+v", result)
	}
	var pending, processing int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status='pending'),
		count(*) FILTER (WHERE status='processing')
		FROM inbox_events`).Scan(&pending, &processing); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || processing != 1 {
		t.Fatalf("remaining pending/processing = %d/%d", pending, processing)
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
	first, err := store.Cleanup(context.Background(), testPolicy(2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if first.WidgetTokens != 2 || first.IdempotencyKeys != 2 {
		t.Fatalf("first bounded result = %+v", first)
	}
	second, err := store.Cleanup(context.Background(), testPolicy(2, 2))
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
		_, err := store.Cleanup(ctx, testPolicy(1, 1))
		firstDone <- err
	}()
	wait.Wait()
	time.Sleep(50 * time.Millisecond)
	second, err := store.Cleanup(ctx, testPolicy(1, 1))
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
