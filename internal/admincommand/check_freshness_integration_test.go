package admincommand

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"sync"
	"testing"
	"time"
)

func TestCheckProjectionFencesVersionsAndLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
		kind     string
		want     string
	}{
		{"success", "", "verified_ok", "verified_ok"},
		{"new oauth", "UPDATE oauth_credentials SET token_version=2 WHERE installation_id=$1", "auth_error", "superseded"},
		{"disabled", "UPDATE installations SET status='disabled' WHERE id=$1", "verified_ok", "superseded"},
		{"uninstalled", "UPDATE installations SET status='uninstalled' WHERE id=$1", "auth_error", "superseded"},
		{"current reauth", "UPDATE installations SET status='reauth_required' WHERE id=$1", "auth_error", "auth_error"},
		{"network", "", "network_error", "network_error"},
		{"rate limited", "", "rate_limited", "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, pool, id, _ := fixture(t)
			_, err := pool.Exec(t.Context(), `INSERT INTO oauth_credentials(installation_id,access_token_ciphertext,refresh_token_ciphertext,expires_at,token_version,key_version)VALUES($1,'a','r',now()+interval '1 hour',1,1)`, id)
			if err != nil {
				t.Fatal(err)
			}
			receipt := execute(t, store, request("installation", id, "check"))
			queue := jobs.NewStore(pool)
			claimed, err := queue.Claim(t.Context(), "check-fixture", 1, time.Minute)
			if err != nil || len(claimed) != 1 {
				t.Fatalf("claim %v %d", err, len(claimed))
			}
			executor := &WorkerExecutor{pool: pool, CheckVersion: func(ctx context.Context, _ uuid.UUID, v int64) (int64, error) {
				if tc.mutation != "" {
					if _, err := pool.Exec(ctx, tc.mutation, id); err != nil {
						t.Fatal(err)
					}
				}
				switch tc.kind {
				case "auth_error":
					return v, &amocrm.APIError{Kind: amocrm.ErrorUnauthorized}
				case "network_error":
					return v, context.DeadlineExceeded
				case "rate_limited":
					return v, &amocrm.APIError{Kind: amocrm.ErrorRateLimited, RetryAfter: time.Hour}
				}
				return v, nil
			}}
			raw, err := executor.Handler(t.Context(), claimed[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := queue.CompleteWithObserver(t.Context(), claimed[0], "check-fixture", raw, time.Millisecond, CompleteReceipt); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(t.Context(), receipt.ID)
			if err != nil || got.Outcome != tc.want {
				t.Fatalf("receipt=%+v err=%v", got, err)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM installation_checks WHERE installation_id=$1`, id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (tc.want == "superseded") != (count == 0) {
				t.Fatalf("projection count=%d", count)
			}
			if tc.kind == "network_error" || tc.kind == "rate_limited" {
				var status string
				_ = pool.QueryRow(t.Context(), `SELECT status FROM installations WHERE id=$1`, id).Scan(&status)
				if status != "active" {
					t.Fatalf("temporary changed status %s", status)
				}
			}
		})
	}
}
func TestSchedulerIdempotentAcrossReplicasAndSmallPool(t *testing.T) {
	store, pool, id, _ := fixture(t)
	cfg := pool.Config()
	cfg.MaxConns = 1
	small, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	store.pool = small
	store.jobs = jobs.NewStore(small)
	now := time.Now().UTC()
	_, err = pool.Exec(t.Context(), `INSERT INTO installation_check_schedule(installation_id,next_check_at)VALUES($1,$2)`, id, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &CheckScheduler{Pool: small, Store: store, Config: CheckConfig{Enabled: true, Interval: time.Hour, Tick: time.Second, Global: 2, Percent: 100}}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Go(func() {
			if err := scheduler.Tick(t.Context(), now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_commands WHERE installation_id=$1 AND command='check'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	// A detached request slot must not starve the one work-pool connection.
	w := &WorkerExecutor{pool: small, GlobalChecks: 1}
	release, err := w.admitCheck(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := small.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil {
		release()
		t.Fatal(err)
	}
	blocked, cancelBlocked := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancelBlocked()
	if second, err := w.admitCheck(blocked, id); err == nil {
		second()
		t.Fatal("overlapping installation check admitted")
	}
	release()
	release, err = w.admitCheck(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestCheckOwnRefreshPublishesNewVersion(t *testing.T) {
	store, pool, id, _ := fixture(t)
	_, err := pool.Exec(t.Context(), `INSERT INTO oauth_credentials(installation_id,access_token_ciphertext,refresh_token_ciphertext,expires_at,token_version,key_version)VALUES($1,'a','r',now()+interval '1 hour',1,1)`, id)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, store, request("installation", id, "check"))
	queue := jobs.NewStore(pool)
	claimed, err := queue.Claim(t.Context(), "check-refresh", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	executor := &WorkerExecutor{pool: pool, CheckVersion: func(ctx context.Context, id uuid.UUID, _ int64) (int64, error) {
		_, err := pool.Exec(ctx, `UPDATE oauth_credentials SET token_version=2 WHERE installation_id=$1`, id)
		return 2, err
	}}
	raw, err := executor.Handler(t.Context(), claimed[0])
	if err != nil {
		t.Fatal(err)
	}
	var result executionResult
	if err := json.Unmarshal(raw, &result); err != nil || result.CheckVersion != 2 {
		t.Fatal("refresh version not captured")
	}
	if err := queue.CompleteWithObserver(t.Context(), claimed[0], "check-refresh", raw, time.Millisecond, CompleteReceipt); err != nil {
		t.Fatal(err)
	}
	var version int64
	if err := pool.QueryRow(t.Context(), `SELECT credential_version FROM installation_checks WHERE installation_id=$1`, id).Scan(&version); err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestCheckBudgetsAcrossInstallationsAndWorkers(t *testing.T) {
	_, pool, id, integration := fixture(t)
	ids := []uuid.UUID{id, uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	var other uuid.UUID
	if err := pool.QueryRow(t.Context(), `INSERT INTO integrations(code,client_id,client_secret_ciphertext,redirect_uri)VALUES('budget-other',$1,'fixture','https://fixture.example.invalid/callback')RETURNING id`, uuid.NewString()).Scan(&other); err != nil {
		t.Fatal(err)
	}
	for n := 1; n < len(ids); n++ {
		owner := integration
		if n >= 3 {
			owner = other
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,$3,'budget.amocrm.test','active')`, ids[n], owner, 91060000+n); err != nil {
			t.Fatal(err)
		}
	}
	first := &WorkerExecutor{pool: pool, GlobalChecks: 3}
	second := &WorkerExecutor{pool: pool, GlobalChecks: 3}
	a, err := first.admitCheck(t.Context(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	defer a()
	b, err := second.admitCheck(t.Context(), ids[1])
	if err != nil {
		t.Fatal(err)
	}
	defer b()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if release, err := second.admitCheck(ctx, ids[2]); err == nil {
		release()
		t.Fatal("integration budget exceeded")
	}
	c, err := second.admitCheck(t.Context(), ids[3])
	if err != nil {
		t.Fatal(err)
	}
	defer c()
	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	if release, err := first.admitCheck(ctx2, ids[4]); err == nil {
		release()
		t.Fatal("global budget exceeded")
	}
}

func TestHourlyCheckPersistDelayBackoffAndRetryAfter(t *testing.T) {
	store, pool, id, _ := fixture(t)
	queue := jobs.NewStore(pool)
	observed := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		kind     string
		retry    int64
		min, max time.Duration
		failures int
	}{
		{"network_error", 0, 48 * time.Minute, 72 * time.Minute, 1},
		{"network_error", 0, 96 * time.Minute, 144 * time.Minute, 2},
		{"internal_error", 0, 192 * time.Minute, 288 * time.Minute, 3},
		{"network_error", 0, 6 * time.Hour, 6 * time.Hour, 4},
		{"rate_limited", 8 * 3600, 8 * time.Hour, 8 * time.Hour, 5},
		{"verified_ok", 0, 48 * time.Minute, 72 * time.Minute, 0},
	} {
		execute(t, store, request("installation", id, "check"))
		claimed, err := queue.Claim(t.Context(), "hourly-check", 1, time.Minute)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim=%d %v", len(claimed), err)
		}
		raw, err := json.Marshal(executionResult{State: "succeeded", Outcome: tc.kind, CheckStatus: "active", CheckObserved: observed, RetryAfter: tc.retry, Result: map[string]any{"classification": tc.kind}})
		if err != nil {
			t.Fatal(err)
		}
		// A zero explicit interval exercises the shared one-hour fallback.
		if err := queue.CompleteWithObserver(t.Context(), claimed[0], "hourly-check", raw, time.Millisecond, CompleteReceipt); err != nil {
			t.Fatal(err)
		}
		var next time.Time
		var failures int
		if err := pool.QueryRow(t.Context(), `SELECT next_check_at,failures FROM installation_checks WHERE installation_id=$1`, id).Scan(&next, &failures); err != nil {
			t.Fatal(err)
		}
		delay := next.Sub(observed)
		if delay < tc.min || delay > tc.max || failures != tc.failures {
			t.Fatalf("%s delay=%s failures=%d", tc.kind, delay, failures)
		}
		observed = observed.Add(10 * time.Hour)
	}
}
