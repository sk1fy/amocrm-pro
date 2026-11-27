package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

type successfulDealSource struct {
	requests []companycrm.DealRequest
}

func (*successfulDealSource) Scope() (int64, string) { return 12, "r1" }

func (s *successfulDealSource) SendInstallationDeal(_ context.Context, request companycrm.DealRequest) (companycrm.DealResult, error) {
	s.requests = append(s.requests, request)
	return companycrm.DealResult{State: "found", ID: 101, Host: "fixture-company.amocrm.ru"}, nil
}

func TestInstallationDealTariffAcceptance(t *testing.T) {
	ownerDSN, jobsDSN, certs := os.Getenv("PLATFORM_TEST_OWNER_DSN"), os.Getenv("PLATFORM_TEST_JOBS_DSN"), os.Getenv("PLATFORM_TEST_CERTS_DIR")
	if ownerDSN == "" || jobsDSN == "" || certs == "" {
		t.Skip("requires a disposable migrated Platform database and generated Platform mTLS certificates")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 1)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, jobsDSN, storage.Jobs, 3)
	mustManager(t, err)
	defer jobs.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes,platform_jobs.jobs CASCADE`)
	mustManager(t, err)
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	configureTariffAcceptanceWorkflow(t, ctx, owner, jobs)

	lifecycle := NewLifecycleStore(jobs)
	platform := filepath.Join(certs, "platform")
	tlsConfig, err := PrivateTLS(filepath.Join(platform, "tls.crt"), filepath.Join(platform, "tls.key"), filepath.Join(platform, "ca.crt"))
	mustManager(t, err)
	server := httptest.NewUnstartedServer(PrivateHandler(LifecycleHandler(lifecycle), nil))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	origin := "https://" + net.JoinHostPort("localhost", strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	client, err := corelifecycle.NewAccountLicenseClient(origin, filepath.Join(certs, "core"))
	mustManager(t, err)
	defer client.Close()

	base := time.Now().UTC().Truncate(time.Microsecond).Add(-25 * time.Hour)
	emit := func(scope wire.ActivationScope, kind wire.Kind, source string) {
		t.Helper()
		event := wire.Event{Version: 1, ID: uuid.New(), Kind: kind, Installation: scope.Installation,
			Integration: scope.Integration, Account: strconv.FormatInt(scope.Account, 10), Generation: scope.Generation,
			Sequence: scope.AuthorizationSequence, OccurredAt: base.Add(time.Duration(scope.AuthorizationSequence) * time.Second),
			Evidence: wire.Evidence{ID: uuid.New(), Source: source}}
		raw, err := json.Marshal(event)
		mustManager(t, err)
		_, err = lifecycle.Record(ctx, raw)
		mustManager(t, err)
	}
	readDeal := func(episode uuid.UUID, strict bool) installationDeal {
		t.Helper()
		item, err := scanManagerInstallation(jobs.QueryRow(ctx, `SELECT `+installationColumns+installationFrom+` WHERE inst.id=$1`, episode), time.Now(), strict)
		mustManager(t, err)
		return *item.Deal
	}
	source := &successfulDealSource{}
	store := queue.NewStore(jobs)
	runUntil := func(episode uuid.UUID, strict bool, wanted queue.Status, attempt int) {
		t.Helper()
		deals := InstallationDeals{Pool: jobs, Source: source, RequireKnownTariff: strict}
		worker := queue.NewWorker(store, map[queue.Kind]queue.Handler{InstallationDealJob: deals.Run}, queue.DefaultConfig(1), prometheus.NewRegistry())
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(workerCtx) }()
		defer func() { stop(); <-done }()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var state queue.Status
			var actualAttempt int
			mustManager(t, owner.QueryRow(ctx, `SELECT status,attempt FROM platform_jobs.jobs WHERE id=$1`, episode).Scan(&state, &actualAttempt))
			if state == wanted && actualAttempt == attempt {
				return
			}
			if state == queue.Dead {
				t.Fatalf("installation deal job exhausted attempts before reaching %s at attempt %d", wanted, attempt)
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("installation deal job is %s at attempt %d, want %s at attempt %d", state, actualAttempt, wanted, attempt)
			}
		}
	}
	activate := func(scope wire.ActivationScope) {
		t.Helper()
		state, err := lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &wire.ActivationContact{
			UserID: 10, Name: "Fixture employee", Phone: "+79001234567", Consent: wire.ContactConsent,
		}})
		mustManager(t, err)
		if !state.Activated {
			t.Fatalf("activation missing for generation %d, sequence %d", scope.Generation, scope.AuthorizationSequence)
		}
	}
	park := func(scope wire.ActivationScope, attempt int) uuid.UUID {
		t.Helper()
		emit(scope, wire.Installed, "core_oauth")
		activate(scope)
		var episode, account uuid.UUID
		mustManager(t, owner.QueryRow(ctx, `SELECT id,account_id FROM platform_manager.installations WHERE installation_id=$1`, scope.Installation).Scan(&episode, &account))
		_, err := owner.Exec(ctx, `INSERT INTO platform_manager.account_profiles(account_id,classification) VALUES($1,'Свободный клиент')
			ON CONFLICT(account_id) DO UPDATE SET classification=EXCLUDED.classification`, account)
		mustManager(t, err)
		_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET attempt=$2-1,max_attempts=$2 WHERE id=$1`, episode, attempt)
		mustManager(t, err)
		runUntil(episode, true, queue.Succeeded, attempt)
		return episode
	}
	license := func(scope wire.ActivationScope, tariff int64, offset time.Duration) wire.AccountLicenseRequest {
		return wire.AccountLicenseRequest{Scope: scope, ReceivedAt: base.Add(offset), Hints: wire.AccountLicenseHints{TariffID: &tariff}}
	}

	scope := wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration, Account: 9007199254740993,
		Generation: 1, AuthorizationSequence: 1}
	episode := park(scope, 5)
	if deal := readDeal(episode, false); deal.State != "pending" || deal.Reason == nil || *deal.Reason != dealTariffUnknown || deal.NextRun != nil || deal.CanRetry {
		t.Fatalf("parked deal after disabling strict waiting: %+v", deal)
	}
	report := license(scope, 100, time.Minute)
	body, err := json.Marshal(report)
	mustManager(t, err)
	rejected, err := tlsClient(t, certs, "manager-bff").Post(origin+wire.AccountLicensePath, "application/json", bytes.NewReader(body))
	mustManager(t, err)
	rejected.Body.Close()
	if rejected.StatusCode != http.StatusForbidden {
		t.Fatalf("account license report with manager identity returned %d, want %d", rejected.StatusCode, http.StatusForbidden)
	}
	if deal := readDeal(episode, false); deal.Reason == nil || *deal.Reason != dealTariffUnknown || len(source.requests) != 0 {
		t.Fatalf("parked deal ran before a trusted tariff report: deal=%+v requests=%d", deal, len(source.requests))
	}
	mustManager(t, client.Report(ctx, report))
	runUntil(episode, false, queue.Succeeded, 6)
	if deal := readDeal(episode, false); deal.State != "succeeded" || deal.ID == nil || *deal.ID != "101" || deal.Reason != nil {
		t.Fatalf("resumed deal was not persisted: %+v", deal)
	}
	if len(source.requests) != 1 {
		t.Fatalf("CRM calls = %d, want 1", len(source.requests))
	}
	request := source.requests[0]
	if !request.Create || request.Episode != episode || request.ClientAccountID != scope.Account || request.Account != 12 ||
		request.Binding != "r1" || request.Pipeline != 100 || request.Stage != 101 || request.ResponsibleUserID != 42 {
		t.Fatalf("resumed job lost its CRM destination or employee: %+v", request)
	}

	scope.AuthorizationSequence++
	emit(scope, wire.Reauthorized, "core_oauth")
	mustManager(t, client.Report(ctx, license(scope, 100, 2*time.Minute)))
	activate(scope)
	scope.AuthorizationSequence++
	emit(scope, wire.Uninstalled, "core_uninstall")
	scope.AuthorizationSequence++
	scope.Generation++
	emit(scope, wire.Reinstalled, "core_oauth")
	mustManager(t, client.Report(ctx, license(scope, 100, 3*time.Minute)))
	activate(scope)
	var total, completed int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE id=$2 AND status='succeeded' AND attempt=6)
		FROM platform_jobs.jobs WHERE kind=$1`, InstallationDealJob, episode).Scan(&total, &completed))
	if total != 1 || completed != 1 {
		t.Fatalf("reauthorization or reinstall rescheduled the deal: total=%d original job still complete=%d", total, completed)
	}
	if deal := readDeal(episode, false); deal.State != "succeeded" || deal.ID == nil || *deal.ID != "101" {
		t.Fatalf("reauthorization or reinstall changed the linked deal: %+v", deal)
	}

	other := wire.ActivationScope{Installation: pilot.otherInstallation, Integration: pilot.otherIntegration, Account: 2542,
		Generation: 1, AuthorizationSequence: 1}
	exhausted := park(other, queue.MaxJobAttempts)
	mustManager(t, client.Report(ctx, license(other, 100, time.Minute)))
	assertExhausted := func(wantTariff int64) {
		t.Helper()
		var state queue.Status
		var code string
		var tariff int64
		mustManager(t, owner.QueryRow(ctx, `SELECT j.status,j.failure_code,t.numeric_value FROM platform_jobs.jobs j
			JOIN platform_manager.installations i ON i.id=j.id
			JOIN platform_manager.account_tariff_fields t ON t.account_id=i.account_id AND t.field='tariff_id'
			WHERE j.id=$1`, exhausted).Scan(&state, &code, &tariff))
		deal := readDeal(exhausted, true)
		if tariff != wantTariff || state != queue.Dead || code != dealTariffResumeExhausted || deal.State != "failed" || !deal.CanRetry || deal.NextRun != nil {
			t.Fatalf("unexpected exhausted deal: tariff=%d want tariff=%d queue=%s code=%s deal=%+v", tariff, wantTariff, state, code, deal)
		}
	}
	assertExhausted(100)
	mustManager(t, client.Report(ctx, license(other, technicalAccountTariffID, 2*time.Minute)))
	assertExhausted(technicalAccountTariffID)
	if len(source.requests) != 1 {
		t.Fatalf("CRM calls after lifecycle changes and exhaustion = %d, want 1", len(source.requests))
	}
}

func configureTariffAcceptanceWorkflow(t *testing.T, ctx context.Context, owner, jobs *pgxpool.Pool) {
	t.Helper()
	var activation companyActivation
	mustManager(t, jobs.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source(12,'r1')`).Scan(&activation.Scope, &activation.Epoch))
	job := uuid.New()
	_, err := owner.Exec(ctx, `INSERT INTO platform_jobs.jobs(id,kind,payload) VALUES($1,$2,'{}')`, job, companyRefreshKind(activation))
	mustManager(t, err)
	_, err = owner.Exec(ctx, `UPDATE platform_company.source_components SET current_job_id=$2 WHERE scope_id=$1 AND component='metadata'`, activation.Scope, job)
	mustManager(t, err)
	pipeline := amocrm.CompanyCRMPipeline{ID: 100, AccountID: 12, Name: "Sales"}
	pipeline.Embedded.Statuses = []amocrm.CompanyCRMStage{{ID: 101, AccountID: 12, PipelineID: 100, Name: "New"}}
	at := time.Now().UTC()
	observation := companyObservation{
		source: companycrm.Source{AccountID: 12, BindingRevision: "r1", State: "fresh", Complete: true, ObservedAt: &at, FreshUntil: new(at.Add(time.Minute))},
		metadata: &companycrm.WorkflowMetadata{Employees: &[]companycrm.WorkflowEmployee{{ID: "42", Name: "Fixture manager"}},
			RequiredFieldsState: "unknown", Pipelines: []amocrm.CompanyCRMPipeline{pipeline}},
	}
	tx, err := jobs.Begin(ctx)
	mustManager(t, err)
	defer tx.Rollback(ctx)
	_, err = publishCompanyObservation(ctx, tx, companyJobPayload{activation, companyMetadata}, job, observation)
	mustManager(t, err)
	mustManager(t, tx.Commit(ctx))
	var generation uuid.UUID
	mustManager(t, jobs.QueryRow(ctx, `SELECT current_generation FROM platform_company.source_components WHERE scope_id=$1 AND component='metadata'`, activation.Scope).Scan(&generation))
	cmd := managerCommand{ID: uuid.New(), Key: uuid.New(), Subject: uuid.New(), RequestID: uuid.New(), Kind: installationDealSettingsCommandKind, Version: 1}
	input := companyWorkflowInput{Owners: &map[string]string{"direct": "42"}, Account: "12", Binding: "r1", Generation: generation, Pipeline: "100", Stage: "101"}
	outcome, err := (managerStore{pool: jobs}).mutate(ctx, cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return saveInstallationDealSettings(ctx, tx, cmd, input)
	})
	mustManager(t, err)
	if outcome.status != http.StatusOK {
		t.Fatalf("fixture workflow returned %d: %s", outcome.status, outcome.body)
	}
}
