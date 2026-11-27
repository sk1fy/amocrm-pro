package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestInstallationDealWaitsForTariff(t *testing.T) {
	ownerDSN, jobsDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN"), os.Getenv("PLATFORM_TEST_JOBS_DSN")
	if ownerDSN == "" || jobsDSN == "" {
		t.Skip("requires a disposable migrated Platform database")
	}
	for _, scenario := range []string{"ordinary before activation", "parked", "observation won", "observation blocked", "technical blocked", "uninstalled", "manual ban"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 1)
			mustManager(t, err)
			defer owner.Close()
			jobs, err := storage.Open(ctx, jobsDSN, storage.Jobs, 3)
			mustManager(t, err)
			defer jobs.Close()
			pilot := managerPilot{ctx: ctx, owner: owner}
			pilot.fixture(t)
			lifecycle := NewLifecycleStore(jobs)
			openedAt := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
			emit := func(kind wire.Kind, sequence int64, at time.Time, source string) {
				t.Helper()
				raw, err := json.Marshal(wire.Event{Version: 1, ID: uuid.New(), Kind: kind, Installation: pilot.installation,
					Integration: pilot.integration, Account: "9007199254740993", Sequence: sequence, Generation: 1, OccurredAt: at,
					Evidence: wire.Evidence{ID: uuid.New(), Source: source}})
				mustManager(t, err)
				_, err = lifecycle.Record(ctx, raw)
				mustManager(t, err)
			}
			emit(wire.Installed, 1, openedAt, "core_oauth")
			scope := wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration, Account: 9007199254740993,
				Generation: 1, AuthorizationSequence: 1}
			var episode, account uuid.UUID
			var scheduled time.Time
			mustManager(t, owner.QueryRow(ctx, `SELECT d.episode_id,inst.account_id,j.available_at FROM platform_manager.installations inst
				JOIN platform_manager.installation_deals d ON d.episode_id=inst.id JOIN platform_jobs.jobs j ON j.id=inst.id
				WHERE inst.installation_id=$1`, pilot.installation).Scan(&episode, &account, &scheduled))
			if !scheduled.Equal(openedAt.Add(24 * time.Hour)) {
				t.Fatalf("base schedule = %s, want %s", scheduled, openedAt.Add(24*time.Hour))
			}
			readDeal := func(at time.Time, strict bool) installationDeal {
				t.Helper()
				item, err := scanManagerInstallation(jobs.QueryRow(ctx, `SELECT `+installationColumns+installationFrom+` WHERE inst.id=$1`, episode), at, strict)
				mustManager(t, err)
				return *item.Deal
			}
			if normal := readDeal(openedAt.Add(24*time.Hour), false); normal.Reason != nil || normal.NextRun == nil {
				t.Fatalf("default schedule changed: %+v", normal)
			}
			if delayed := readDeal(openedAt.Add(24*time.Hour), true); delayed.Reason == nil || *delayed.Reason != dealTariffUnknown || delayed.NextRun != nil {
				t.Fatalf("delayed job missing strict reason: %+v", delayed)
			}
			observe := func(observedAt time.Time, tariff int64) error {
				return lifecycle.RecordAccountLicense(ctx, wire.AccountLicenseRequest{Scope: scope, ReceivedAt: observedAt,
					Hints: wire.AccountLicenseHints{TariffID: &tariff}})
			}
			if scenario == "ordinary before activation" {
				mustManager(t, observe(openedAt, 100))
				if normal := readDeal(openedAt, true); normal.NextRun == nil || !normal.NextRun.Equal(scheduled) || normal.Reason != nil {
					t.Fatalf("tariff observation accelerated the base schedule: %+v", normal)
				}
			}
			_, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &wire.ActivationContact{
				UserID: 10, Name: "Fixture employee", Phone: "+79001234567", Consent: wire.ContactConsent,
			}})
			mustManager(t, err)
			deals := InstallationDeals{Pool: jobs, Source: &installationDealStub{}, RequireKnownTariff: true}
			if scenario == "ordinary before activation" {
				if activated := readDeal(openedAt, true); activated.NextRun == nil || !activated.NextRun.Before(scheduled) || activated.Reason != nil {
					t.Fatalf("activation did not accelerate the known tariff: %+v", activated)
				}
				_, decision, err := deals.reserve(ctx, episode)
				mustManager(t, err)
				if decision == dealTariffUnknown {
					t.Fatal("fresh ordinary tariff did not pass the gate")
				}
				return
			}
			_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET attempt=4 WHERE id=$1`, episode)
			mustManager(t, err)

			queueStore := queue.NewStore(jobs)
			ready, release, resumed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			blockedBy := make(chan int32, 1)
			workerCtx, stop := context.WithCancel(ctx)
			workerDone := make(chan struct{})
			workerConfig := queue.DefaultConfig(1)
			workerConfig.QueryTimeout = 8 * time.Second
			blocked := scenario == "observation blocked" || scenario == "technical blocked"
			first := true
			worker := queue.NewWorker(queueStore, map[queue.Kind]queue.Handler{InstallationDealJob: func(ctx context.Context, job queue.Job) queue.Completion {
				if job.ID != episode {
					return queue.Completion{}
				}
				if !first {
					close(resumed)
					<-ctx.Done()
					return queue.Completion{}
				}
				first = false
				completion := deals.Run(ctx, job)
				if blocked && completion.Publish != nil {
					publish := completion.Publish
					completion.Publish = func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
						outcome, err := publish(ctx, tx)
						blockedBy <- int32(tx.Conn().PgConn().PID())
						close(ready)
						select {
						case <-release:
						case <-ctx.Done():
						}
						return outcome, err
					}
				} else {
					close(ready)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return completion
			}}, workerConfig, prometheus.NewRegistry())
			go func() { defer close(workerDone); worker.Run(workerCtx) }()
			defer func() { stop(); <-workerDone }()
			waitSignal := func(signal <-chan struct{}) {
				t.Helper()
				select {
				case <-signal:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			poll := func(check func() bool) {
				t.Helper()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for !check() {
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			waitSignal(ready)
			if scenario == "observation won" {
				mustManager(t, observe(openedAt, 100))
			}
			if blocked {
				tariff := int64(100)
				if scenario == "technical blocked" {
					tariff = technicalAccountTariffID
				}
				observationDone := make(chan error, 1)
				go func() { observationDone <- observe(openedAt, tariff) }()
				workerPID := <-blockedBy
				poll(func() bool {
					var waiting bool
					mustManager(t, owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity
						WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, workerPID).Scan(&waiting))
					return waiting
				})
				close(release)
				mustManager(t, <-observationDone)
			} else {
				close(release)
			}
			jobState := func() (state string, attempt, maxAttempts int) {
				t.Helper()
				mustManager(t, owner.QueryRow(ctx, `SELECT status,attempt,max_attempts FROM platform_jobs.jobs WHERE id=$1`, episode).Scan(&state, &attempt, &maxAttempts))
				return
			}
			if scenario == "parked" || scenario == "uninstalled" || scenario == "manual ban" {
				poll(func() bool { state, _, _ := jobState(); return state == "succeeded" })
				before := readDeal(openedAt.Add(24*time.Hour-time.Microsecond), true)
				after := readDeal(openedAt.Add(24*time.Hour), true)
				if before.State != "pending" || before.Reason != nil || before.NextRun != nil || before.CanRetry ||
					after.State != "pending" || after.Reason == nil || *after.Reason != dealTariffUnknown || after.NextRun != nil || after.CanRetry {
					t.Fatalf("wrong visibility around 24 hours: before=%+v after=%+v", before, after)
				}
				mustManager(t, observe(openedAt.Add(-time.Microsecond), 100))
				if state, attempt, maxAttempts := jobState(); state != "succeeded" || attempt != 5 || maxAttempts != 5 {
					t.Fatalf("old tariff woke the job: %s %d/%d", state, attempt, maxAttempts)
				}
				if scenario == "uninstalled" {
					emit(wire.Uninstalled, 2, openedAt.Add(30*time.Second), "core_uninstall")
					if deal := readDeal(openedAt.Add(24*time.Hour), true); deal.State != "skipped" || deal.Reason != nil {
						t.Fatalf("removed installation still waits: %+v", deal)
					}
					return
				}
				if scenario == "manual ban" {
					patch, err := parseProfilePatch([]byte(`{"classification":"Запрет работы","reason":"fixture manual ban"}`))
					mustManager(t, err)
					cmd := managerCommand{ID: uuid.New(), Key: uuid.New(), Subject: uuid.New(), RequestID: uuid.New(), Account: account, Kind: profileCommandKind, Version: 1}
					outcome, err := (managerStore{pool: jobs}).mutate(ctx, cmd, patch, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
						return updateAccountProfile(ctx, tx, cmd, patch)
					})
					mustManager(t, err)
					mustManager(t, observe(openedAt, 100))
					if deal := readDeal(openedAt.Add(24*time.Hour), true); outcome.status != 200 || deal.State != "skipped" || deal.Reason != nil {
						t.Fatalf("manual ban left the deal parked or woke it: status=%d deal=%+v", outcome.status, deal)
					}
					return
				}
				mustManager(t, observe(openedAt, 100))
			}
			if scenario == "technical blocked" {
				deal := readDeal(openedAt, true)
				if deal.State != "skipped" || deal.Reason == nil || *deal.Reason != dealTechnicalAccount || deal.CanRetry {
					t.Fatalf("technical tariff left the deal parked: %+v", deal)
				}
				return
			}
			waitSignal(resumed)
			if state, attempt, maxAttempts := jobState(); state != "running" || attempt != 6 || maxAttempts != 6 {
				t.Fatalf("tariff did not restore a claim with a fresh fence: %s %d/%d", state, attempt, maxAttempts)
			}
			if _, err := queueStore.Finish(ctx, queue.Lease{Job: queue.Job{ID: episode}, Attempt: 5, MaxAttempts: 5}, nil); !errors.Is(err, queue.ErrLeaseLost) {
				t.Fatalf("old fence completed the resumed job: %v", err)
			}
		})
	}
}
