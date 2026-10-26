package gateway

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
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
	if _, err := store.Read(ctx, scope, req); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("initial loading must not be zero: %v", err)
	}
	// 21 complete pages: there is no old 20-page/5000-record ceiling.
	for page := 1; page <= 21; page++ {
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
		if _, err = store.Read(ctx, scope, req); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
			t.Fatalf("staging data published on page %d", page)
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
	if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()`); err != nil {
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
	deadline := time.Now().Add(5 * time.Second)
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
