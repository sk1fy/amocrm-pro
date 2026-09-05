package jobs

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

// The fixture is intentionally independent of fairClaimQuery, so changes to the
// production selector do not silently move the measurement baseline.
//
//go:embed testdata/fair_claim_baseline.sql
var fairClaimBaselineSQL string

type fairBenchmarkCase struct {
	Name                  string `json:"name"`
	Integrations          int    `json:"integrations"`
	ActiveIntegrations    int    `json:"active_integrations"`
	InstallationsPerLane  int    `json:"installations_per_lane"`
	ReadyJobs             int    `json:"ready_jobs"`
	FutureJobs            int    `json:"future_high_priority_jobs"`
	PlatformJobs          int    `json:"platform_jobs"`
	SaturatedIntegrations int    `json:"saturated_integrations"`
	ExpiredJobs           int    `json:"expired_jobs"`
	ExhaustedJobs         int    `json:"exhausted_jobs"`
	Cap                   int    `json:"integration_cap"`
}

var fairBenchmarkCases = []fairBenchmarkCase{
	{Name: "ready_100k", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 100000, Cap: 64},
	{Name: "short_cap2", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 10000, Cap: 2},
	{Name: "idle_1000_lanes", Integrations: 1000, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 10000, Cap: 64},
	{Name: "600_installations", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 300, ReadyJobs: 12000, Cap: 64},
	{Name: "90_saturated_lanes", Integrations: 100, ActiveIntegrations: 100, InstallationsPerLane: 1, ReadyJobs: 10000, SaturatedIntegrations: 90, Cap: 10},
	{Name: "future_priority_100k", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 10000, FutureJobs: 100000, Cap: 64},
	{Name: "platform_and_100k", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 100000, PlatformJobs: 1000, Cap: 64},
	{Name: "reaping_with_observer", Integrations: 2, ActiveIntegrations: 2, InstallationsPerLane: 1, ReadyJobs: 10000, ExpiredJobs: 20, ExhaustedJobs: 20, Cap: 64},
}

type fairBenchmarkSample struct {
	Wave            int     `json:"wave"`
	Claimer         int     `json:"claimer"`
	ReturnedJobs    int     `json:"returned_jobs"`
	WallMS          float64 `json:"wall_ms"`
	LockQueryMS     float64 `json:"lock_query_ms"`
	LockHoldMS      float64 `json:"lock_hold_ms"`
	SlotQueryMS     float64 `json:"slot_query_ms"`
	SlotQueryMaxMS  float64 `json:"slot_query_max_ms"`
	NonSlotQueryMS  float64 `json:"non_slot_query_ms"`
	ReapQueryMS     float64 `json:"reap_query_ms"`
	ObserverQueryMS float64 `json:"observer_query_ms"`
	CommitQueryMS   float64 `json:"commit_query_ms"`
	OtherQueryMS    float64 `json:"other_query_ms"`
	Statements      int     `json:"statements"`
	ObservedJobs    int     `json:"observed_jobs"`
	WaveElapsedMS   float64 `json:"wave_elapsed_ms"`
}

type fairBenchmarkQuantiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

type fairBenchmarkResult struct {
	Scenario     fairBenchmarkCase      `json:"scenario"`
	Path         string                 `json:"path"`
	Claimers     int                    `json:"claimers"`
	Batch        int                    `json:"batch"`
	WarmupWaves  int                    `json:"warmup_waves"`
	Samples      []fairBenchmarkSample  `json:"samples"`
	ReturnedJobs int                    `json:"returned_jobs"`
	WallMS       fairBenchmarkQuantiles `json:"wall_ms"`
	LockQueryMS  fairBenchmarkQuantiles `json:"lock_query_ms"`
	LockHoldMS   fairBenchmarkQuantiles `json:"lock_hold_ms"`
}

// TestFairClaimPerformance is opt-in even when TEST_DATABASE_URL is configured.
// It measures admission transactions only: no job handler is executed. Run Go
// and PostgreSQL in the project's Docker environment, with the normal testkit
// reset safeguards plus FAIR_CLAIM_BENCHMARK=true. The selected database is reset.
// FAIR_CLAIM_BENCHMARK_OUTPUT is required and receives exact seeds, plans, raw
// samples, metadata and summaries. SAMPLES counts synchronized waves per path
// and concurrency, after two excluded warmup waves. CASES is a comma-separated
// filter; PATHS accepts baseline,current. Each wave pairs both paths on restored
// seed state and reverses their order on alternate waves. No DDL changes occur.
func TestFairClaimPerformance(t *testing.T) {
	if os.Getenv("FAIR_CLAIM_BENCHMARK") != "true" {
		t.Skip("opt-in admission benchmark; set FAIR_CLAIM_BENCHMARK=true")
	}
	output := os.Getenv("FAIR_CLAIM_BENCHMARK_OUTPUT")
	if output == "" {
		t.Fatal("FAIR_CLAIM_BENCHMARK_OUTPUT is required to preserve plans and raw samples")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	waves := fairBenchmarkPositiveEnv(t, "FAIR_CLAIM_BENCHMARK_SAMPLES", 12)
	guardPool := testkit.Postgres(t) // Validate reset permission and hold the suite lock.
	cfg := guardPool.Config()
	cfg.MaxConns, cfg.MinConns = 10, 8
	cfg.ConnConfig.Tracer = fairBenchmarkTracer{}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fairBenchmarkMetadata(t, pool, output)
	var results []fairBenchmarkResult
	for _, scenario := range fairBenchmarkCases {
		if !fairBenchmarkSelected(os.Getenv("FAIR_CLAIM_BENCHMARK_CASES"), scenario.Name) {
			continue
		}
		for _, claimers := range []int{1, 8} {
			initialOrder := []string{"baseline", "current"}
			if claimers == 8 {
				initialOrder = []string{"current", "baseline"}
			}
			var paths []string
			for _, path := range initialOrder {
				if fairBenchmarkSelected(os.Getenv("FAIR_CLAIM_BENCHMARK_PATHS"), path) {
					paths = append(paths, path)
				}
			}
			if len(paths) == 0 {
				continue
			}
			t.Run(fmt.Sprintf("%s/%d", scenario.Name, claimers), func(t *testing.T) {
				testkit.Reset(t, guardPool)
				seed := fairBenchmarkSeed(scenario)
				fairBenchmarkWrite(t, filepath.Join(output, scenario.Name+".seed.sql"), []byte(seed))
				fairBenchmarkExec(t, pool, seed)
				fairBenchmarkVacuum(t, pool)
				byPath := make(map[string]*fairBenchmarkResult, len(paths))
				const warmups = 2
				for _, path := range paths {
					query := fairClaimBaselineSQL
					if path == "current" {
						query = fairClaimQuery
					}
					name := fmt.Sprintf("%s.%d.%s", scenario.Name, claimers, path)
					fairBenchmarkPlans(t, pool, scenario, query, output, name)
					byPath[path] = &fairBenchmarkResult{Scenario: scenario, Path: path, Claimers: claimers, Batch: 10, WarmupWaves: warmups}
					fairBenchmarkVacuum(t, pool) // EXPLAIN ANALYZE rolls back its writes.
				}
				for wave := -warmups; wave < waves; wave++ {
					// Pair variants within each wave so time drift is not confounded
					// with the implementation. A wave uses A/B, then B/A next wave.
					for index := range paths {
						if wave%2 != 0 {
							index = len(paths) - 1 - index
						}
						path := paths[index]
						result := byPath[path]
						samples, ids := fairBenchmarkWave(t, pool, scenario, path, claimers, result.Batch, wave)
						if wave >= 0 {
							result.Samples = append(result.Samples, samples...)
						}
						fairBenchmarkRestore(t, pool, ids)
						fairBenchmarkVacuum(t, pool)
					}
				}
				for _, path := range paths {
					result := byPath[path]
					result.summarize()
					name := fmt.Sprintf("%s.%d.%s", scenario.Name, claimers, path)
					fairBenchmarkJSON(t, filepath.Join(output, name+".json"), result)
					results = append(results, *result)
					t.Logf("%s admission only: returned=%d transactions=%d wall p50/p95=%.2f/%.2fms lock query=%.2f/%.2fms hold=%.2f/%.2fms", path, result.ReturnedJobs, len(result.Samples), result.WallMS.P50, result.WallMS.P95, result.LockQueryMS.P50, result.LockQueryMS.P95, result.LockHoldMS.P50, result.LockHoldMS.P95)
				}
			})
		}
	}
	if len(results) == 0 {
		t.Fatal("no scenarios selected")
	}
	fairBenchmarkJSON(t, filepath.Join(output, "results.json"), results)
}

// pgx tracing measures the client-observed lock query duration, including one
// network round trip and execution overhead (NOT pure PostgreSQL lock wait).
// Hold is measured from that query's completion through COMMIT's completion,
// also including the commit round trip. Wall includes pool acquisition and BEGIN.
// These boundaries work without modifying the production claim API.
type fairBenchmarkTrace struct {
	queryStarted     time.Time
	queryKind        string
	lockStarted      time.Time
	lockAcquired     time.Time
	committed        time.Time
	statements       int
	slotQuerySum     time.Duration
	slotQueryMax     time.Duration
	reapQuerySum     time.Duration
	observerQuerySum time.Duration
	commitQuerySum   time.Duration
	otherQuerySum    time.Duration
}

type fairBenchmarkTraceKey struct{}
type fairBenchmarkTracer struct{}

func (fairBenchmarkTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if trace, ok := ctx.Value(fairBenchmarkTraceKey{}).(*fairBenchmarkTrace); ok {
		trace.queryStarted = time.Now()
		trace.queryKind = fairBenchmarkQueryKind(data.SQL)
		trace.statements++
		if strings.Contains(data.SQL, "pg_advisory_xact_lock") {
			trace.lockStarted = trace.queryStarted
		}
	}
	return ctx
}

func (fairBenchmarkTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if trace, ok := ctx.Value(fairBenchmarkTraceKey{}).(*fairBenchmarkTrace); ok && data.Err == nil {
		ended := time.Now()
		elapsed := ended.Sub(trace.queryStarted)
		if trace.queryStarted == trace.lockStarted {
			trace.lockAcquired = ended
		}
		if data.CommandTag.String() == "COMMIT" {
			trace.committed = ended
		}
		switch trace.queryKind {
		case "slot":
			trace.slotQuerySum += elapsed
			trace.slotQueryMax = max(trace.slotQueryMax, elapsed)
		case "reap":
			trace.reapQuerySum += elapsed
		case "observer":
			trace.observerQuerySum += elapsed
		case "commit":
			trace.commitQuerySum += elapsed
		case "lock":
			// The existing lock_query_ms already measures this statement.
		default:
			trace.otherQuerySum += elapsed
		}
	}
}

func fairBenchmarkQueryKind(sql string) string {
	if sql == fairClaimQuery || sql == fairClaimBaselineSQL {
		return "slot"
	}
	if strings.Contains(sql, "pg_advisory_xact_lock") {
		return "lock"
	}
	normalized := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case normalized == "commit":
		return "commit"
	case strings.HasPrefix(normalized, "insert into audit_log"):
		return "observer"
	case strings.HasPrefix(normalized, "update jobs"),
		strings.HasPrefix(normalized, "insert into job_attempts"),
		strings.HasPrefix(normalized, "select") && strings.Contains(normalized, "from jobs"):
		return "reap"
	default:
		return "other"
	}
}

func fairBenchmarkWave(t *testing.T, pool *pgxpool.Pool, scenario fairBenchmarkCase, path string, claimers, batch, wave int) ([]fairBenchmarkSample, []uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	samples, jobLists, errs := make([]fairBenchmarkSample, claimers), make([][]Job, claimers), make([]error, claimers)
	for claimer := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trace := &fairBenchmarkTrace{}
			claimCtx := context.WithValue(ctx, fairBenchmarkTraceKey{}, trace)
			observed := 0
			var observer FailureObserver
			if scenario.ExpiredJobs+scenario.ExhaustedJobs > 0 {
				observer = func(ctx context.Context, tx TxExecutor, job Job, failure Failure, _ Status) error {
					_, err := tx.Exec(ctx, `INSERT INTO audit_log(actor_type,action,object_type,object_id,metadata)
                        VALUES ('benchmark','benchmark.reaped','job',$1,jsonb_build_object('failure',$2::text))`, job.ID.String(), failure.Code)
					if err == nil {
						observed++
					}
					return err
				}
			}
			<-start
			began := time.Now()
			worker := fmt.Sprintf("benchmark-%d", claimer)
			if path == "baseline" {
				jobLists[claimer], errs[claimer] = fairBenchmarkBaselineClaim(claimCtx, pool, worker, batch, scenario.Cap, observer)
			} else {
				jobLists[claimer], errs[claimer] = NewStore(pool).ClaimFairWithObserver(claimCtx, worker, batch, 100, time.Hour, scenario.Cap, observer)
			}
			samples[claimer] = fairBenchmarkSample{Wave: wave, Claimer: claimer, ReturnedJobs: len(jobLists[claimer]), WallMS: fairBenchmarkMS(time.Since(began)), Statements: trace.statements, ObservedJobs: observed}
			samples[claimer].SlotQueryMS = fairBenchmarkMS(trace.slotQuerySum)
			samples[claimer].SlotQueryMaxMS = fairBenchmarkMS(trace.slotQueryMax)
			samples[claimer].ReapQueryMS = fairBenchmarkMS(trace.reapQuerySum)
			samples[claimer].ObserverQueryMS = fairBenchmarkMS(trace.observerQuerySum)
			samples[claimer].CommitQueryMS = fairBenchmarkMS(trace.commitQuerySum)
			samples[claimer].OtherQueryMS = fairBenchmarkMS(trace.otherQuerySum)
			samples[claimer].NonSlotQueryMS = fairBenchmarkMS(trace.reapQuerySum + trace.observerQuerySum + trace.commitQuerySum + trace.otherQuerySum)
			if errs[claimer] == nil {
				if trace.lockAcquired.IsZero() || trace.committed.IsZero() {
					errs[claimer] = errors.New("claim tracing did not observe lock acquisition and commit")
					return
				}
				samples[claimer].LockQueryMS = fairBenchmarkMS(trace.lockAcquired.Sub(trace.lockStarted))
				samples[claimer].LockHoldMS = fairBenchmarkMS(trace.committed.Sub(trace.lockAcquired))
			}
		}()
	}
	waveStarted := time.Now()
	close(start)
	wg.Wait()
	waveMS := fairBenchmarkMS(time.Since(waveStarted))
	ids := make([]uuid.UUID, 0, claimers*batch)
	seen := make(map[uuid.UUID]bool)
	observed := 0
	for claimer, err := range errs {
		if err != nil {
			t.Fatalf("wave %d claimer %d: %v", wave, claimer, err)
		}
		for _, job := range jobLists[claimer] {
			if job.Type == "benchmark.future" || job.Type == "benchmark.saturated" {
				t.Fatalf("claimed ineligible job: %s", job.Type)
			}
			if seen[job.ID] {
				t.Fatalf("job %s returned more than once in wave %d", job.ID, wave)
			}
			seen[job.ID] = true
			ids = append(ids, job.ID)
		}
		samples[claimer].WaveElapsedMS = waveMS
		observed += samples[claimer].ObservedJobs
	}
	capacity := (scenario.ActiveIntegrations - scenario.SaturatedIntegrations) * scenario.Cap
	if scenario.PlatformJobs > 0 {
		capacity += scenario.Cap
	}
	expected := min(claimers*batch, capacity, scenario.ReadyJobs+scenario.PlatformJobs+scenario.ExpiredJobs)
	if len(ids) != expected {
		t.Fatalf("wave %d returned %d jobs, want %d across %d claimers", wave, len(ids), expected, claimers)
	}
	if observed != scenario.ExpiredJobs+scenario.ExhaustedJobs {
		t.Fatalf("observer called %d times, want %d", observed, scenario.ExpiredJobs+scenario.ExhaustedJobs)
	}
	var persisted, overCap int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='benchmark.reaped'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != observed {
		t.Fatalf("observer writes committed=%d calls=%d", persisted, observed)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (
        SELECT i.integration_id FROM jobs j LEFT JOIN installations i ON i.id=j.installation_id
        WHERE j.status='processing' AND j.locked_until>=statement_timestamp()
        GROUP BY i.integration_id HAVING count(*)>$1) exceeded`, scenario.Cap).Scan(&overCap); err != nil {
		t.Fatal(err)
	}
	if overCap != 0 {
		t.Fatalf("%d lanes exceeded cap", overCap)
	}
	return samples, ids
}

// Freeze the old transaction's sequential round trips too. Reapers are shared
// intentionally; selector A/B runs always use the same migrated schema/reapers.
func fairBenchmarkBaselineClaim(ctx context.Context, pool *pgxpool.Pool, worker string, batch, cap int, observer FailureObserver) ([]Job, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, fairClaimLockID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO job_queue_lanes(scope_id) VALUES ('00000000-0000-0000-0000-000000000000') ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}
	if err := reapExpired(ctx, tx, 100, observer); err != nil {
		return nil, err
	}
	if err := reapExhausted(ctx, tx, 100, observer); err != nil {
		return nil, err
	}
	claimed := make([]Job, 0, batch)
	for range batch {
		job, err := scanJob(tx.QueryRow(ctx, fairClaimBaselineSQL, cap, worker, time.Hour.Milliseconds()))
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, job)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

func fairBenchmarkSeed(s fairBenchmarkCase) string {
	// UUIDs, timestamps and tie breaking are deterministic; leases/future jobs use
	// 2999 so wall clock progression cannot change scenario eligibility mid-run.
	return fmt.Sprintf(`-- Deterministic admission benchmark seed. Not an execution workload.
INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri,created_at,updated_at)
SELECT md5('integration:'||n)::uuid,'benchmark-'||n,'benchmark-'||n,'secret','https://example.test/callback','2020-01-01','2020-01-01'
FROM generate_series(1,%[1]d) n;
INSERT INTO installations(id,integration_id,account_id,account_domain,status,created_at,updated_at)
SELECT md5('installation:'||n||':'||k)::uuid,md5('integration:'||n)::uuid,k,'benchmark.amocrm.ru','active','2020-01-01','2020-01-01'
FROM generate_series(1,%[1]d) n CROSS JOIN generate_series(1,%[2]d) k;
INSERT INTO job_queue_lanes(scope_id) VALUES ('00000000-0000-0000-0000-000000000000') ON CONFLICT DO NOTHING;
INSERT INTO jobs(id,installation_id,type,priority,run_after,created_at,updated_at)
SELECT md5('ready:'||n)::uuid,md5('installation:'||((n-1) %% %[3]d+1)||':'||(((n-1)/%[3]d) %% %[2]d+1))::uuid,
       'benchmark.ready',100,'2020-01-02','2020-01-01','2020-01-01'
FROM generate_series(1,%[4]d) n;
INSERT INTO jobs(id,installation_id,type,priority,run_after,created_at,updated_at)
SELECT md5('future:'||n)::uuid,md5('installation:'||((n-1) %% %[3]d+1)||':1')::uuid,
       'benchmark.future',1,'2999-01-01','2020-01-01','2020-01-01'
FROM generate_series(1,%[5]d) n;
INSERT INTO jobs(id,type,priority,run_after,created_at,updated_at)
SELECT md5('platform:'||n)::uuid,'benchmark.platform',100,'2020-01-02','2020-01-01','2020-01-01'
FROM generate_series(1,%[6]d) n;
INSERT INTO jobs(id,installation_id,type,status,attempts,locked_by,locked_until,run_after,created_at,updated_at)
SELECT md5('saturated:'||n||':'||k)::uuid,md5('installation:'||n||':1')::uuid,
       'benchmark.saturated','processing',1,'preexisting-worker','2999-01-01','2020-01-02','2020-01-01','2020-01-01'
FROM generate_series(1,%[7]d) n CROSS JOIN generate_series(1,%[8]d) k;
INSERT INTO jobs(id,installation_id,type,status,attempts,locked_by,locked_until,run_after,created_at,updated_at)
SELECT md5('expired:'||n)::uuid,md5('installation:'||((n-1) %% %[3]d+1)||':1')::uuid,
       'benchmark.expired','processing',1,'expired-worker','2020-01-03','2020-01-02','2020-01-01','2020-01-01'
FROM generate_series(1,%[9]d) n;
INSERT INTO jobs(id,installation_id,type,status,attempts,run_after,created_at,updated_at)
SELECT md5('exhausted:'||n)::uuid,md5('installation:'||((n-1) %% %[3]d+1)||':1')::uuid,
       'benchmark.exhausted','retry',5,'2020-01-02','2020-01-01','2020-01-01'
FROM generate_series(1,%[10]d) n;
`, s.Integrations, s.InstallationsPerLane, s.ActiveIntegrations, s.ReadyJobs, s.FutureJobs, s.PlatformJobs, s.SaturatedIntegrations, s.Cap, s.ExpiredJobs, s.ExhaustedJobs)
}

func fairBenchmarkRestore(t *testing.T, pool *pgxpool.Pool, ids []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='queued',attempts=0,locked_by=NULL,locked_until=NULL,
        run_after='2020-01-02',last_error_code=NULL,last_error_message=NULL,finished_at=NULL WHERE id=ANY($1::uuid[])`, ids); err != nil {
		t.Fatal(err)
	}
	fairBenchmarkExec(t, pool, `
UPDATE jobs SET status='processing',attempts=1,locked_by='expired-worker',locked_until='2020-01-03',
    run_after='2020-01-02',last_error_code=NULL,last_error_message=NULL,finished_at=NULL WHERE type='benchmark.expired';
UPDATE jobs SET status='retry',attempts=5,last_error_code=NULL,last_error_message=NULL,finished_at=NULL WHERE type='benchmark.exhausted';
UPDATE job_queue_lanes SET last_claimed_at='-infinity';
TRUNCATE job_attempts,audit_log RESTART IDENTITY;`)
}

func fairBenchmarkVacuum(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// Equalize visibility map/dead tuples between waves. These operations are
	// outside the measured window and describe a warm, controlled baseline.
	for _, table := range []string{"jobs", "installations", "job_queue_lanes"} {
		fairBenchmarkExec(t, pool, "VACUUM (ANALYZE) "+table)
	}
}

func fairBenchmarkPlans(t *testing.T, pool *pgxpool.Pool, scenario fairBenchmarkCase, query, output, name string) {
	t.Helper()
	queries := []struct {
		name string
		sql  string
		args []any
	}{
		{"claim", query, []any{scenario.Cap, "explain-worker", time.Hour.Milliseconds()}},
		{"reap_expired", `SELECT ` + jobColumns + ` FROM jobs WHERE status='processing' AND locked_until<now()
            ORDER BY locked_until,created_at FOR UPDATE SKIP LOCKED LIMIT $1`, []any{100}},
		{"reap_exhausted", `SELECT ` + jobColumns + ` FROM jobs WHERE status IN ('queued','retry') AND attempts>=max_attempts
            ORDER BY run_after,created_at FOR UPDATE SKIP LOCKED LIMIT $1`, []any{100}},
	}
	for _, q := range queries {
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, fairClaimLockID); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS, FORMAT TEXT) "+q.sql, q.args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				plan.WriteString(line + "\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			fairBenchmarkWrite(t, filepath.Join(output, name+"."+q.name+".plan.txt"), []byte(plan.String()))
		}()
	}
}

func fairBenchmarkMetadata(t *testing.T, pool *pgxpool.Pool, output string) {
	t.Helper()
	ctx := context.Background()
	metadata := map[string]any{
		"started_at_utc":   time.Now().UTC(),
		"go_version":       runtime.Version(),
		"goos":             runtime.GOOS,
		"goarch":           runtime.GOARCH,
		"gomaxprocs":       runtime.GOMAXPROCS(0),
		"experiment_order": "Paired by wave on one deterministic seed per scenario/concurrency, with restore and VACUUM ANALYZE between every path. Even waves use baseline/current for one claimer and current/baseline for eight claimers; odd waves reverse the order. Two paired warmup waves (-2,-1) are excluded. Each path retains its own samples and result file.",
		"measurement":      "Admission only; no handler execution, completion or end-to-end throughput is measured. Each wave resets claimed rows; 100k denotes backlog size, not completed jobs.",
		"timing":           "Client wall includes pool/BEGIN/COMMIT; lock_query includes PostgreSQL wait plus execution and network round trip; lock_hold runs from lock-query response through COMMIT response. p95 is nearest-rank. Samples are synchronized claimer waves after two excluded warmup waves; VACUUM ANALYZE and resets are outside measured time.",
		"timing_breakdown": "Per-sample slot_query_ms is the sum of slot statement client durations; slot_query_max_ms is the slowest slot statement. non_slot_query_ms excludes the scheduler lock and equals reap_query_ms (lease/exhaustion SELECTs, updates and attempt inserts) + observer_query_ms (audit inserts) + commit_query_ms + other_query_ms (BEGIN, platform lane ensure and any unclassified SQL). All query durations include network and pgx row consumption, not pure server execution; application time between statements is not included.",
		"baseline":         "Frozen original selector and sequential transaction; same current reapers and migrated schema as current. Any index DDL experiment must be run separately and identified by its metadata indexes.",
		"scope":            "Synthetic Docker database, warm cache; not production workload and not pure server lock-wait instrumentation.",
	}
	for _, setting := range []string{"server_version", "shared_buffers", "work_mem", "max_connections", "jit", "random_page_cost", "effective_cache_size", "max_parallel_workers_per_gather", "track_io_timing"} {
		var value string
		if err := pool.QueryRow(ctx, "SHOW "+setting).Scan(&value); err != nil {
			t.Fatal(err)
		}
		metadata[setting] = value
	}
	var indexes []string
	rows, err := pool.Query(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='public' AND tablename IN ('jobs','installations','job_queue_lanes') ORDER BY tablename,indexname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	metadata["indexes"] = indexes
	var migrations []string
	rows, err = pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		migrations = append(migrations, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	metadata["migration_versions"] = migrations
	for name, query := range map[string]string{"baseline": fairClaimBaselineSQL, "current": fairClaimQuery} {
		hash := sha256.Sum256([]byte(query))
		metadata[name+"_sql_sha256"] = hex.EncodeToString(hash[:])
		fairBenchmarkWrite(t, filepath.Join(output, name+".sql"), []byte(query))
	}
	fairBenchmarkJSON(t, filepath.Join(output, "metadata.json"), metadata)
}

func (r *fairBenchmarkResult) summarize() {
	wall, query, hold := make([]float64, 0, len(r.Samples)), make([]float64, 0, len(r.Samples)), make([]float64, 0, len(r.Samples))
	for _, sample := range r.Samples {
		r.ReturnedJobs += sample.ReturnedJobs
		wall, query, hold = append(wall, sample.WallMS), append(query, sample.LockQueryMS), append(hold, sample.LockHoldMS)
	}
	r.WallMS, r.LockQueryMS, r.LockHoldMS = fairBenchmarkPercentiles(wall), fairBenchmarkPercentiles(query), fairBenchmarkPercentiles(hold)
}

func fairBenchmarkPercentiles(values []float64) fairBenchmarkQuantiles {
	sort.Float64s(values)
	return fairBenchmarkQuantiles{P50: values[int(math.Ceil(float64(len(values))*.50))-1], P95: values[int(math.Ceil(float64(len(values))*.95))-1]}
}

func fairBenchmarkMS(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func fairBenchmarkSelected(filter, value string) bool {
	if filter == "" {
		return true
	}
	for _, option := range strings.Split(filter, ",") {
		if strings.TrimSpace(option) == value {
			return true
		}
	}
	return false
}

func fairBenchmarkPositiveEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	if raw := os.Getenv(name); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			t.Fatalf("%s must be a positive integer", name)
		}
		return value
	}
	return fallback
}

func fairBenchmarkExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

func fairBenchmarkJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fairBenchmarkWrite(t, path, append(data, '\n'))
}

func fairBenchmarkWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
