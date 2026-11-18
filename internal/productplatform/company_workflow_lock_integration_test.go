package productplatform

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformCompanyCatalogLockOrder(t *testing.T) {
	requireCompanyProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	store := managerStore{jobs}
	subject := uuid.New()
	command := func(kind string, version int64) managerCommand {
		return managerCommand{ID: uuid.New(), Key: uuid.New(), Subject: subject, RequestID: uuid.New(), Product: pilot.product, Kind: kind, Version: version}
	}
	create := command(productCreateCommandKind, 0)
	create.Resource, create.Product = pilot.product, uuid.Nil
	input, err := parseProductInput([]byte(fmt.Sprintf(`{"code":"pilot-review","display_name":"Catalog lock fixture","line":"PRO","availability":"pilot","integration_refs":[%q],"tariff_version":null,"trial_policy":{"kind":"once","duration_days":7},"links":[]}`, pilot.integration)), true)
	mustManager(t, err)
	outcome, err := store.mutate(ctx, create, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return saveProduct(ctx, tx, create, input, true)
	})
	mustManager(t, err)
	if outcome.status != 201 {
		t.Fatalf("adoption status %d", outcome.status)
	}
	var activation companyActivation
	mustManager(t, jobs.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source(12,'r1')`).Scan(&activation.Scope, &activation.Epoch))
	job := uuid.New()
	_, err = owner.Exec(ctx, `INSERT INTO platform_jobs.jobs(id,kind,payload) VALUES($1,$2,'{}')`, job, companyRefreshKind(activation))
	mustManager(t, err)
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET current_job_id=$2 WHERE scope_id=$1 AND component='metadata'`, activation.Scope, job)
	mustManager(t, err)
	at := time.Now().UTC().Truncate(time.Microsecond)
	until := at.Add(time.Minute)
	pipeline := amocrm.CompanyCRMPipeline{ID: 100, AccountID: 12, Name: "Sales"}
	pipeline.Embedded.Statuses = []amocrm.CompanyCRMStage{{ID: 101, AccountID: 12, PipelineID: 100, Name: "New"}}
	observation := companyObservation{source: companycrm.Source{AccountID: 12, BindingRevision: "r1", State: "fresh", Complete: true, ObservedAt: &at, FreshUntil: &until}, metadata: &companycrm.WorkflowMetadata{RequiredFieldsState: "unknown", Pipelines: []amocrm.CompanyCRMPipeline{pipeline}}}
	tx, err := jobs.Begin(ctx)
	mustManager(t, err)
	_, err = publishCompanyObservation(ctx, tx, companyJobPayload{activation, companyMetadata}, job, observation)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	mustManager(t, tx.Commit(ctx))
	var generation uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT current_generation FROM platform_company.source_components WHERE scope_id=$1 AND component='metadata'`, activation.Scope).Scan(&generation))
	catalogCommand, workflowCommand := command(productUpdateCommandKind, 1), command(companyWorkflowCommandKind, 1)
	catalogInput, err := parseProductInput([]byte(`{"display_name":"Catalog after concurrent save"}`), false)
	mustManager(t, err)
	workflowInput := companyWorkflowInput{Account: "12", Binding: "r1", Generation: generation, Pipeline: "100", Stage: "101"}
	catalogReady, workflowReady := make(chan uint32, 1), make(chan uint32, 1)
	releaseCatalog, releaseWorkflow := make(chan struct{}), make(chan struct{})
	type result struct {
		kind    string
		outcome managerOutcome
		err     error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	defer func() { cancel(); group.Wait() }()
	group.Go(func() {
		outcome, err := store.mutate(ctx, catalogCommand, catalogInput, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
			catalogReady <- tx.Conn().PgConn().PID()
			select {
			case <-releaseCatalog:
			case <-ctx.Done():
				return managerOutcome{}, ctx.Err()
			}
			return saveProduct(ctx, tx, catalogCommand, catalogInput, false)
		})
		results <- result{"catalog", outcome, err}
	})
	group.Go(func() {
		outcome, err := store.mutate(ctx, workflowCommand, workflowInput, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
			workflowReady <- tx.Conn().PgConn().PID()
			select {
			case <-releaseWorkflow:
			case <-ctx.Done():
				return managerOutcome{}, ctx.Err()
			}
			return saveCompanyWorkflow(ctx, tx, workflowCommand, workflowInput)
		})
		results <- result{"workflow", outcome, err}
	})
	var catalogPID, workflowPID uint32
	select {
	case catalogPID = <-catalogReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case workflowPID = <-workflowReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(releaseCatalog)
	companyWait(t, func() bool {
		var blocked bool
		e := owner.QueryRow(ctx, `SELECT $2::integer=ANY(pg_blocking_pids($1))`, catalogPID, workflowPID).Scan(&blocked)
		return e == nil && blocked
	})
	close(releaseWorkflow)
	for range 2 {
		select {
		case r := <-results:
			if r.err != nil || r.outcome.status != 200 {
				t.Errorf("%s outcome=%d error=%v", r.kind, r.outcome.status, r.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
