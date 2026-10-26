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
	"sync"
	"testing"
	"time"
)

func TestProblemSnapshotBeyond5000ResumesAndPublishesOnlyCompleteGeneration(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'secret','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	store := &ProblemStore{pool: pool}
	req := serviceapi.ProblemRequest{EmployeeIDs: []int64{7}}
	if err := store.Request(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Read(ctx, scope, req); err != nil || !got.Collecting || got.SnapshotAt != 0 || len(got.Counts) != 0 {
		t.Fatalf("initial collection must not be verified zero: %+v %v", got, err)
	}
	// 21 complete pages: there is no old 20-page/5000-record ceiling.
	for page := 1; page <= 21; page++ {
		if page == 21 {
			if _, err := store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("5000-record boundary did not pause: %v", err)
			}
			var due time.Time
			if err := pool.QueryRow(ctx, `SELECT next_poll_at FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&due); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				if err := store.Request(ctx, scope); err != nil {
					t.Fatal(err)
				}
			}
			var after time.Time
			if err := pool.QueryRow(ctx, `SELECT next_poll_at FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !due.Equal(after) || time.Until(due) < 55*time.Second {
				t.Fatal("requests shortened batch pause")
			}
			if _, err := pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()`); err != nil {
				t.Fatal(err)
			}
		}
		if page == 11 {
			store = &ProblemStore{pool: pool}
		} // process restart
		c, err := store.claim(ctx)
		if err != nil || c.page != page || c.phase != "leads" {
			t.Fatalf("checkpoint page %d claim %+v err %v", page, c, err)
		}
		rows := make([]amocrm.ProblemLead, 250)
		for i := range rows {
			rows[i] = amocrm.ProblemLead{ID: int64((page-1)*250 + i + 1), ResponsibleUserID: 7, PipelineID: 10, StatusID: 11}
		}
		if err = store.apply(ctx, c, amocrm.ProblemPage{Leads: rows, HasNext: page < 21}); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Read(ctx, scope, req); err != nil || got.SnapshotAt != 0 || len(got.Counts) != 0 || !got.Collecting {
			t.Fatalf("staging data published on page %d: %+v %v", page, got, err)
		}
	}
	c, err := store.claim(ctx)
	if err != nil || c.phase != "tasks" || c.page != 1 {
		t.Fatalf("task phase %+v %v", c, err)
	}
	if err = store.apply(ctx, c, amocrm.ProblemPage{Tasks: []amocrm.ProblemTask{{ID: 1, EntityType: "leads", EntityID: 1, ResponsibleUserID: 7, CompleteTill: time.Now().Add(-time.Hour).Unix()}}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(ctx, scope, req)
	if err != nil || got.Counts[0].LeadsNoTasks != 5249 || got.Counts[0].LeadsOverdueTasks != 1 || got.Counts[0].OverdueTasks != 1 {
		t.Fatalf("completed snapshot %+v %v", got, err)
	}
	filtered, err := store.Read(ctx, scope, serviceapi.ProblemRequest{EmployeeIDs: []int64{7, 9}, PipelineIDs: []int64{20}})
	if err != nil || len(filtered.Counts) != 2 || filtered.Counts[0].LeadsNoTasks != 0 || filtered.Counts[1].LeadsNoTasks != 0 {
		t.Fatalf("pipeline scope %+v %v", filtered, err)
	}
	foreign := scope
	foreign.IntegrationID = uuid.New()
	if _, err = store.Read(ctx, foreign, req); err == nil {
		t.Fatal("cross integration snapshot accepted")
	}
	if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("idle snapshot auto-refreshed: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET snapshot_at=now()-interval '7 minutes',next_poll_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err = store.Request(ctx, scope); err != nil {
		t.Fatal(err)
	}
	c, err = store.claim(ctx)
	if err != nil || c.phase != "leads" || c.page != 1 {
		t.Fatal("next generation not started")
	}
	store.fail(ctx, c, serviceapi.Fail(serviceapi.Unavailable, "upstream temporarily unavailable"))
	got, err = store.Read(ctx, scope, req)
	if err != nil || got.Counts[0].LeadsNoTasks != 5249 || !got.Stale {
		t.Fatalf("failure replaced good snapshot %+v %v", got, err)
	}
	// Exercise the running collector and its live Core admission, not only storage helpers.
	if _, err = pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'activity',true)`, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO activity_pilots(installation_id,enabled) VALUES($1,true)`, scope.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()`); err != nil {
		t.Fatal(err)
	}
	workerAPI := &snapshotWorkerAPI{}
	service := New(workerAPI, nil).WithProblemStore(pool)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.RunProblemCollector(workerCtx) }()
	// This checks eventual durable completion, not a five-second latency SLO.
	// Allow loaded CI hosts to schedule the collector between pages.
	deadline := time.Now().Add(30 * time.Second)
	complete := false
	for time.Now().Before(deadline) {
		current, e := store.Read(ctx, scope, req)
		if e != nil {
			t.Fatal(e)
		}
		if current.Counts[0].LeadsNoTasks == 1 {
			complete = true
			break
		}
		if current.Counts[0].LeadsNoTasks != 5249 {
			t.Fatal("worker exposed incomplete generation")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatalf("worker shutdown %v", e)
	}
	if !complete || workerAPI.calls != 3 {
		t.Fatalf("worker did not finish all pages: complete=%t calls=%d", complete, workerAPI.calls)
	}

}

type snapshotWorkerAPI struct {
	API
	calls int
}

func (a *snapshotWorkerAPI) ReadProblemPage(_ context.Context, _ uuid.UUID, phase string, page int) (amocrm.ProblemPage, error) {
	a.calls++
	if phase == "leads" {
		return amocrm.ProblemPage{Leads: []amocrm.ProblemLead{{ID: int64(1000 + page), ResponsibleUserID: 7, PipelineID: 10, StatusID: 11}}, HasNext: page == 1}, nil
	}
	return amocrm.ProblemPage{Tasks: []amocrm.ProblemTask{{ID: 5, ResponsibleUserID: 7, EntityID: 1001, EntityType: "leads", CompleteTill: time.Now().Add(-time.Hour).Unix()}}}, nil
}

// Concurrent tabs serialize Request without overwriting a live checkpoint or
// spawning additional generations. An elapsed deadline alone is not demand.
func TestProblemDemandRefreshConcurrentTabsAndSevenMinuteBoundary(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'secret','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	store := &ProblemStore{pool: pool}
	concurrentRequests := func() {
		t.Helper()
		var wg sync.WaitGroup
		failures := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := store.Request(ctx, scope); err != nil {
					failures <- err
				}
			}()
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Fatal(err)
		}
	}
	concurrentRequests()
	c, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second worker acquired leased collection: %v", err)
	}
	concurrentRequests()
	if err = store.apply(ctx, c, amocrm.ProblemPage{}); err != nil {
		t.Fatal(err)
	}
	c, err = store.claim(ctx)
	if err != nil || c.phase != "tasks" {
		t.Fatalf("task claim %+v %v", c, err)
	}
	if err = store.apply(ctx, c, amocrm.ProblemPage{}); err != nil {
		t.Fatal(err)
	}
	var generation uuid.UUID
	var rowVersion string
	if err = pool.QueryRow(ctx, `SELECT staging_generation,xmin::text FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&generation, &rowVersion); err != nil {
		t.Fatal(err)
	}
	concurrentRequests()
	var after uuid.UUID
	var versionAfter string
	if err = pool.QueryRow(ctx, `SELECT staging_generation,xmin::text FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&after, &versionAfter); err != nil {
		t.Fatal(err)
	}
	if generation != after || rowVersion != versionAfter {
		t.Fatal("fresh requests rewrote the snapshot row")
	}
	for _, age := range []string{"6 minutes 59 seconds", "7 minutes"} {
		if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET snapshot_at=now()-$1::interval,next_poll_at=now()-interval '1 hour'`, age); err != nil {
			t.Fatal(err)
		}
		if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("idle auto-refresh at %s: %v", age, err)
		}
		concurrentRequests()
		c, err = store.claim(ctx)
		if age == "6 minutes 59 seconds" {
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal("fresh snapshot refreshed early")
			}
			continue
		}
		if err != nil || c.generation == generation || c.page != 1 || c.phase != "leads" {
			t.Fatalf("expired snapshot not refreshed: %+v %v", c, err)
		}
	}
	// Retry-After survives repeated requests and restart; the same page resumes.
	store.fail(ctx, c, &amocrm.APIError{Kind: amocrm.ErrorRateLimited, StatusCode: 429, RetryAfter: 90 * time.Second})
	concurrentRequests()
	var due time.Time
	if err = pool.QueryRow(ctx, `SELECT next_poll_at FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.Until(due) < 85*time.Second {
		t.Fatalf("Retry-After ignored: %s", time.Until(due))
	}
	store = &ProblemStore{pool: pool}
	if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("retry pause bypassed: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()`); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.claim(ctx)
	if err != nil || resumed.generation != c.generation || resumed.phase != c.phase || resumed.page != c.page {
		t.Fatalf("checkpoint not resumed: %+v %v", resumed, err)
	}
	// A former owner cannot apply or fail after another process reclaims its lease.
	if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	newer, err := store.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.apply(ctx, resumed, amocrm.ProblemPage{}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("lost lease applied: %v", err)
	}
	store.fail(ctx, resumed, serviceapi.Fail(serviceapi.Unavailable, "old owner"))
	if err = store.apply(ctx, newer, amocrm.ProblemPage{}); err != nil {
		t.Fatal(err)
	}
}

func TestProblemBatchCountsRowsAcrossLeadsAndTasks(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'secret','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	store := &ProblemStore{pool: pool}
	if err := store.Request(ctx, scope); err != nil {
		t.Fatal(err)
	}
	// 4600 leads: 18 full pages and one short final page.
	for page := 1; page <= 19; page++ {
		c, err := store.claim(ctx)
		if err != nil || c.page != page || c.phase != "leads" {
			t.Fatalf("lead checkpoint %+v %v", c, err)
		}
		size := 250
		if page == 19 {
			size = 100
		}
		leads := make([]amocrm.ProblemLead, size)
		for i := range leads {
			leads[i] = amocrm.ProblemLead{ID: int64((page-1)*250 + i + 1), ResponsibleUserID: 7, PipelineID: 10, StatusID: 11}
		}
		if err = store.apply(ctx, c, amocrm.ProblemPage{Leads: leads, HasNext: page < 19}); err != nil {
			t.Fatal(err)
		}
	}
	for page := 1; page <= 2; page++ {
		if page == 2 {
			// 4600 lead rows +250 task rows leave less than one full page:
			// pause at4850, rather than fetching a page that would exceed5000.
			if _, err := store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cross-phase quota did not pause: %v", err)
			}
			store = &ProblemStore{pool: pool} // Restart during pause.
			if err := store.Request(ctx, scope); err != nil {
				t.Fatal(err)
			}
			if _, err := store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("restart/request bypassed pause: %v", err)
			}
			if _, err := pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()`); err != nil {
				t.Fatal(err)
			}
		}
		c, err := store.claim(ctx)
		if err != nil || c.phase != "tasks" || c.page != page {
			t.Fatalf("task checkpoint %+v %v", c, err)
		}
		tasks := make([]amocrm.ProblemTask, 250)
		for i := range tasks {
			tasks[i] = amocrm.ProblemTask{ID: int64((page-1)*250 + i + 1), ResponsibleUserID: 7, EntityType: "leads", EntityID: int64((page-1)*250 + i + 1), CompleteTill: time.Now().Add(-time.Hour).Unix()}
		}
		if err = store.apply(ctx, c, amocrm.ProblemPage{Tasks: tasks, HasNext: page < 2}); err != nil {
			t.Fatal(err)
		}
		got, err := store.Read(ctx, scope, serviceapi.ProblemRequest{EmployeeIDs: []int64{7}})
		if err != nil {
			t.Fatal(err)
		}
		if page == 1 && (got.SnapshotAt != 0 || len(got.Counts) != 0 || !got.Collecting) {
			t.Fatal("partial task page published")
		}
		if page == 2 && (got.SnapshotAt == 0 || got.Collecting || got.Counts[0].LeadsNoTasks != 4100 || got.Counts[0].LeadsOverdueTasks != 500) {
			t.Fatalf("complete %+v", got)
		}
	}
	if _, err := store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("complete generation auto-restarted: %v", err)
	}
}
