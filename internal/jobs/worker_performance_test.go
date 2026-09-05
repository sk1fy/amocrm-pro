package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

// This opt-in experiment actually executes and durably completes all 24 jobs.
// Both schedulers use the same production claim SQL, handlers and finalization;
// only completion wakeups differ. It is not a 100,000-job execution benchmark.
func TestWorkerPollingPerformance(t *testing.T) {
	if os.Getenv("FAIR_CLAIM_BENCHMARK") != "true" {
		t.Skip("opt-in worker timing; set FAIR_CLAIM_BENCHMARK=true")
	}
	pool := testkit.Postgres(t)
	const jobCount = 24
	output := os.Getenv("FAIR_CLAIM_BENCHMARK_OUTPUT")
	if output != "" {
		if err := os.MkdirAll(output, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := WorkerConfig{
		ID: "polling-performance", PollInterval: time.Second, LeaseDuration: time.Minute,
		JobTimeout: time.Minute, BatchSize: 10, ReapBatchSize: 100, Concurrency: 4,
		IntegrationConcurrency: 2, DrainTimeout: 2 * time.Second, ClaimTimeout: 2 * time.Second,
	}
	handler := func(ctx context.Context, _ Job) (json.RawMessage, error) {
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return json.RawMessage(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var samples []workerPollingSample
	t.Log("same production SQL; jobs=24 integrations=2 integration_cap=2 concurrency=4 batch=10 poll=1s handler=50ms repeats=3; seed time excluded")
	for repeat := range 3 {
		modes := []string{"ticker_only", "slot_wakeup"}
		if repeat%2 != 0 {
			modes[0], modes[1] = modes[1], modes[0]
		}
		for _, mode := range modes {
			testkit.Reset(t, pool)
			for range 2 {
				installation := seedQueueInstallation(t, pool, uuid.New(), 1)
				seedQueueLoad(t, pool, installation, jobCount/2, 100)
			}
			completionLog := &workerCompletionLog{target: jobCount, done: make(chan struct{})}
			worker := NewWorker(NewStore(pool), slog.New(completionLog), config,
				map[string]Handler{"workflow.lead.set_status": handler})
			elapsed := measureWorkerPolling(t, worker, mode, completionLog.done)
			var completed, attempts, completedAttempts int
			if err := pool.QueryRow(context.Background(), `
				SELECT count(*) FILTER (WHERE status='completed'), sum(attempts),
				       (SELECT count(*) FROM job_attempts WHERE outcome='completed') FROM jobs`).
				Scan(&completed, &attempts, &completedAttempts); err != nil {
				t.Fatal(err)
			}
			if completed != jobCount || attempts != jobCount || completedAttempts != jobCount {
				t.Fatalf("incomplete run: completed=%d attempts=%d completed_attempts=%d", completed, attempts, completedAttempts)
			}
			t.Logf("mode=%s repeat=%d wall=%s completed=%d attempts=%d completed_attempts=%d jobs_per_second=%.2f",
				mode, repeat+1, elapsed, completed, attempts, completedAttempts, float64(completed)/elapsed.Seconds())
			samples = append(samples, workerPollingSample{
				Mode: mode, Repeat: repeat + 1, WallMS: fairBenchmarkMS(elapsed),
				Completed: completed, Attempts: attempts, CompletedAttempts: completedAttempts,
			})
		}
	}
	if output != "" {
		summary := make(map[string]workerPollingSummary)
		for _, mode := range []string{"ticker_only", "slot_wakeup"} {
			var times []float64
			var completed int
			for _, sample := range samples {
				if sample.Mode == mode {
					times = append(times, sample.WallMS)
					completed += sample.Completed
				}
			}
			sort.Float64s(times)
			summary[mode] = workerPollingSummary{
				Runs: len(times), TotalCompleted: completed,
				MinMS: times[0], MedianMS: times[len(times)/2], MaxMS: times[len(times)-1],
			}
		}
		fairBenchmarkJSON(t, filepath.Join(output, "worker-results.json"), map[string]any{
			"measurement": "24 jobs actually executed and durably completed per run; same production SQL; seed time excluded; order alternated; not a 100k-job benchmark",
			"config": map[string]int64{
				"jobs_per_run": jobCount, "integrations": 2, "integration_cap": int64(config.IntegrationConcurrency),
				"concurrency": int64(config.Concurrency), "batch_size": int64(config.BatchSize),
				"reap_batch_size": int64(config.ReapBatchSize), "poll_interval_ms": config.PollInterval.Milliseconds(),
				"handler_duration_ms": 50, "repeats_per_mode": 3,
				"lease_duration_ms": config.LeaseDuration.Milliseconds(), "job_timeout_ms": config.JobTimeout.Milliseconds(),
				"claim_timeout_ms": config.ClaimTimeout.Milliseconds(), "drain_timeout_ms": config.DrainTimeout.Milliseconds(),
			},
			"samples": samples, "summary": summary,
		})
	}
}

type workerPollingSample struct {
	Mode              string  `json:"mode"`
	Repeat            int     `json:"repeat"`
	WallMS            float64 `json:"wall_ms"`
	Completed         int     `json:"completed"`
	Attempts          int     `json:"attempts"`
	CompletedAttempts int     `json:"completed_attempts"`
}

type workerPollingSummary struct {
	Runs           int     `json:"runs"`
	TotalCompleted int     `json:"total_completed"`
	MinMS          float64 `json:"min_wall_ms"`
	MedianMS       float64 `json:"median_wall_ms"`
	MaxMS          float64 `json:"max_wall_ms"`
}

func measureWorkerPolling(t *testing.T, worker *Worker, mode string, completed <-chan struct{}) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	started := time.Now()
	go func() {
		if mode == "ticker_only" {
			workerDone <- runTickerOnlyWorker(ctx, worker)
		} else {
			workerDone <- worker.Run(ctx)
		}
	}()
	finished := false
	select {
	case <-completed:
		finished = true
	case <-ctx.Done():
	}
	elapsed := time.Since(started)
	cancel()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop after performance run")
	}
	if !finished {
		t.Fatalf("%s did not complete all jobs before deadline", mode)
	}
	return elapsed
}

// The old worker admitted work at startup and on ticks. BatchSize >= Concurrency
// makes poll issue at most one claim, matching its original behavior exactly.
func runTickerOnlyWorker(ctx context.Context, worker *Worker) error {
	ticker := time.NewTicker(worker.config.PollInterval)
	defer ticker.Stop()
	semaphore := make(chan struct{}, worker.config.Concurrency)
	var active sync.WaitGroup
	worker.poll(ctx, semaphore, &active, nil)
	for {
		select {
		case <-ctx.Done():
			worker.drain(&active)
			return nil
		case <-ticker.C:
			worker.poll(ctx, semaphore, &active, nil)
		}
	}
}

// workerCompletionLog observes completion after Store.Complete commits without
// extra SQL polling. Database counts are independently checked afterwards.
type workerCompletionLog struct {
	completed atomic.Int64
	target    int64
	done      chan struct{}
}

func (*workerCompletionLog) Enabled(context.Context, slog.Level) bool { return true }
func (log *workerCompletionLog) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "job completed" && log.completed.Add(1) == log.target {
		close(log.done)
	}
	return nil
}
func (log *workerCompletionLog) WithAttrs([]slog.Attr) slog.Handler { return log }
func (log *workerCompletionLog) WithGroup(string) slog.Handler      { return log }
