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
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type dealUserObserver func(*platformpb.ObserveAccountRequest) (*platformpb.AccountObservation, error)

func (f dealUserObserver) ObserveAccount(_ context.Context, request *platformpb.ObserveAccountRequest, _ ...grpc.CallOption) (*platformpb.AccountObservation, error) {
	return f(request)
}

type installationDealStub struct {
	requests []companycrm.DealRequest
}

func (*installationDealStub) Scope() (int64, string) { return 12, "r1" }
func (stub *installationDealStub) SendInstallationDeal(_ context.Context, request companycrm.DealRequest) (companycrm.DealResult, error) {
	stub.requests = append(stub.requests, request)
	if len(stub.requests) == 1 {
		return companycrm.DealResult{}, errors.New("provider accepted the request but the response was lost")
	}
	return companycrm.DealResult{State: "found", ID: 101, Host: "fixture-company.amocrm.ru"}, nil
}

func TestInstallationActivationAndUncertainDeal(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_OWNER_DSN") == "" || os.Getenv("PLATFORM_TEST_JOBS_DSN") == "" {
		t.Skip("requires a disposable migrated Platform database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pool, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer pool.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_company.source_scopes CASCADE`)
	mustManager(t, err)
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	store := managerStore{pool}
	subject := uuid.New()
	command := func(kind string, version int64) managerCommand {
		return managerCommand{ID: uuid.New(), Key: uuid.New(), Subject: subject, RequestID: uuid.New(), Product: pilot.product, Kind: kind, Version: version}
	}
	catalog := command(productCreateCommandKind, 0)
	catalog.Resource, catalog.Product = pilot.product, uuid.Nil
	input, err := parseProductInput([]byte(`{"code":"pilot-review","display_name":"Pilot widget","line":"PRO","availability":"pilot","integration_refs":["`+pilot.integration.String()+`"],"tariff_version":null,"trial_policy":{"kind":"once","duration_days":7},"links":[]}`), true)
	mustManager(t, err)
	created, err := store.mutate(ctx, catalog, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return saveProduct(ctx, tx, catalog, input, true)
	})
	mustManager(t, err)
	if created.status != 201 {
		t.Fatalf("catalog adoption returned %d", created.status)
	}

	var activation companyActivation
	mustManager(t, pool.QueryRow(ctx, `SELECT scope_id,epoch FROM platform_company.register_source(12,'r1')`).Scan(&activation.Scope, &activation.Epoch))
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
	tx, err := pool.Begin(ctx)
	mustManager(t, err)
	_, err = publishCompanyObservation(ctx, tx, companyJobPayload{activation, companyMetadata}, job, observation)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	mustManager(t, tx.Commit(ctx))
	var generation uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT current_generation FROM platform_company.source_components WHERE scope_id=$1 AND component='metadata'`, activation.Scope).Scan(&generation))
	workflow := command(installationDealSettingsCommandKind, 1)
	workflow.Product = uuid.Nil
	workflowInput := companyWorkflowInput{Account: "12", Binding: "r1", Generation: generation, Pipeline: "100", Stage: "101"}
	saved, err := store.mutate(ctx, workflow, workflowInput, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return saveInstallationDealSettings(ctx, tx, workflow, workflowInput)
	})
	mustManager(t, err)
	if saved.status != 200 {
		t.Fatalf("saving workflow returned %d: %s", saved.status, saved.body)
	}

	lifecycle := NewLifecycleStore(pool)
	write := func(kind wire.Kind, sequence, generation int64) {
		source := "core_oauth"
		if kind == wire.Uninstalled {
			source = "core_uninstall"
		}
		event := wire.Event{ID: uuid.New(), Version: 1, Kind: kind, Installation: pilot.installation, Integration: pilot.integration,
			Account: "9007199254740993", Sequence: sequence, Generation: generation, OccurredAt: at.Add(time.Duration(sequence-1) * time.Second),
			Evidence: wire.Evidence{ID: uuid.New(), Source: source}}
		raw, err := json.Marshal(event)
		mustManager(t, err)
		_, err = lifecycle.Record(ctx, raw)
		mustManager(t, err)
	}
	write(wire.Installed, 1, 1)
	scope := wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration, Account: 9007199254740993,
		Generation: 1, AuthorizationSequence: 1}
	state, err := lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope})
	mustManager(t, err)
	if state.Activated {
		t.Fatal("preview reported an unsubmitted contact")
	}
	phone := "+79001234567"
	contact := wire.ActivationContact{UserID: 10, Name: "Fixture employee", Email: "staff@example.invalid", Role: "Admin", Phone: phone, Consent: wire.ContactConsent}
	state, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &contact})
	mustManager(t, err)
	if !state.Activated {
		t.Fatal("activation was not recorded")
	}
	second := contact
	second.UserID, second.Phone = 11, "+79000000000"
	_, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &second})
	mustManager(t, err)
	found, _, err := store.list(ctx, managerActor{Subject: subject}, installationFilters{Limit: 25, Q: "8 (900) 123-45-67"}, installationSeek{})
	mustManager(t, err)
	if len(found.Items) != 1 || found.Items[0].Contact == nil || found.Items[0].Contact.UserID != "10" || found.Items[0].Contact.Phone != phone ||
		found.Items[0].Activation != "activated" || found.Items[0].Deal == nil || found.Items[0].Deal.State != "pending" {
		t.Fatalf("activation did not keep the first contact or search by phone: %+v", found)
	}
	episode := found.Items[0].ID
	review, err := store.review(ctx, reviewCommand{ID: uuid.New(), Key: uuid.New(), Episode: episode, Subject: subject,
		RequestID: uuid.New(), Version: found.Items[0].Version, Input: reviewInput{Reviewed: true}})
	mustManager(t, err)
	var receipt managerInstallation
	mustManager(t, json.Unmarshal(review.body, &receipt))
	if review.status != 200 || receipt.Contact != nil || !receipt.Review.Reviewed {
		t.Fatal("write-only review receipt must omit the contact")
	}
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_manager.account_identities(account_id,current_domain,display_name,observed_at)
		SELECT account_id,'fixture-account.amocrm.ru','Fixture account',statement_timestamp()
		FROM platform_manager.installations WHERE id=$1`, episode)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_manager.account_rosters(account_id,roster,active_count,evidence_id,observed_at,fresh_until)
		SELECT account_id,'[]'::jsonb,0,$2,statement_timestamp(),statement_timestamp()+interval '1 hour'
		FROM platform_manager.installations WHERE id=$1`, episode, uuid.New())
	mustManager(t, err)
	zone, err := time.LoadLocation("Europe/Moscow")
	mustManager(t, err)
	date := at.In(zone).Format("02.01.2006")
	fake := &installationDealStub{}
	var observedAccounts []int64
	gateway := dealUserObserver(func(request *platformpb.ObserveAccountRequest) (*platformpb.AccountObservation, error) {
		observedAccounts = append(observedAccounts, request.AmoAccountId)
		if request.Component != platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS {
			t.Fatalf("unexpected observation type: %s", request.Component)
		}
		if request.AmoAccountId != 2542 {
			return nil, status.Error(codes.Unavailable, "fixture gateway unavailable")
		}
		return &platformpb.AccountObservation{ObservedAt: timestamppb.Now(), Result: &platformpb.AccountObservation_Roster{
			Roster: &platformpb.CRMUserRoster{Users: []*platformpb.ObservedCRMUser{
				{Id: 1, Name: "Fixture employee", IsActive: true},
				{Id: 2, Name: "Second fixture employee", IsActive: true},
				{Id: 3, Name: "Inactive fixture employee"},
			}},
		}}, nil
	})
	service := InstallationDeals{Pool: pool, Source: fake, Manager: &ManagerOptions{zone: zone}, Accounts: NewAccountObserver(pool, gateway)}
	run := func() {
		completion := service.Run(ctx, queue.Job{ID: episode, Kind: InstallationDealJob})
		if completion.Publish == nil {
			t.Fatalf("deal completion has no result: %+v", completion)
		}
		tx, err := pool.Begin(ctx)
		mustManager(t, err)
		_, err = completion.Publish(ctx, tx)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		mustManager(t, tx.Commit(ctx))
	}
	run()
	_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET status='dead' WHERE id=$1`, episode)
	mustManager(t, err)
	retry := dealRetryCommand{ID: uuid.New(), Subject: subject, Key: uuid.New(), Episode: episode, Version: 1, Reason: "fixture reconciliation"}
	_, err = store.retryInstallationDeal(ctx, retry)
	mustManager(t, err)
	_, err = store.retryInstallationDeal(ctx, retry)
	mustManager(t, err)
	run()
	if len(fake.requests) != 2 || !fake.requests[0].Create || fake.requests[1].Create ||
		fake.requests[0].Title != found.Items[0].ProductName+" · fixture-account · Fixture employee · "+date ||
		fake.requests[0].Title != fake.requests[1].Title || fake.requests[0].Pipeline != 100 || fake.requests[0].Stage != 101 {
		t.Fatalf("uncertain request was not reconciled by its original title: %+v", fake.requests)
	}
	if fake.requests[0].ClientAccountID != scope.Account || fake.requests[0].ActiveUsers == nil || *fake.requests[0].ActiveUsers != 0 ||
		fake.requests[1].ClientAccountID != 0 || fake.requests[1].ActiveUsers != nil || len(observedAccounts) != 0 {
		t.Fatalf("account enrichment did not distinguish creation, zero users and reconciliation: %+v", fake.requests)
	}
	_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET status='succeeded' WHERE id=$1`, episode)
	mustManager(t, err)
	result, err := store.readDealOperation(ctx, retry.ID, subject)
	mustManager(t, err)
	if result.State != "succeeded" {
		t.Fatalf("operation did not expose completion: %+v", result)
	}
	view, _, err := store.list(ctx, managerActor{Subject: subject}, installationFilters{Limit: 25}, installationSeek{})
	mustManager(t, err)
	if len(view.Items) != 1 || view.Items[0].Deal == nil || view.Items[0].Deal.URL == nil ||
		*view.Items[0].Deal.URL != "https://fixture-company.amocrm.ru/leads/detail/101" {
		t.Fatalf("successful deal link missing from the queue: %+v", view)
	}

	// A second product has no product-specific configuration.
	otherEvent := wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Installed, Installation: pilot.otherInstallation,
		Integration: pilot.otherIntegration, Account: "2542", Sequence: 1, Generation: 1, OccurredAt: at,
		Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}}
	raw, err := json.Marshal(otherEvent)
	mustManager(t, err)
	_, err = lifecycle.Record(ctx, raw)
	mustManager(t, err)
	var otherEpisode uuid.UUID
	var otherProductName string
	mustManager(t, owner.QueryRow(ctx, `
		SELECT d.episode_id,i.product_name FROM platform_manager.installation_deals d
		JOIN platform_manager.installations i ON i.id=d.episode_id
		WHERE i.amo_account_id=2542`).Scan(&otherEpisode, &otherProductName))
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_manager.account_rosters(account_id,roster,active_count,evidence_id,observed_at,fresh_until)
		SELECT account_id,'[{"id":"1","name":"Fixture employee","is_active":true,"is_admin":false}]'::jsonb,
			1,$2,statement_timestamp()-interval '2 days',statement_timestamp()-interval '1 day'
		FROM platform_manager.installations WHERE id=$1`, otherEpisode, uuid.New())
	mustManager(t, err)
	episode = otherEpisode
	run()
	if len(fake.requests) != 3 || !fake.requests[2].Create ||
		fake.requests[2].Title != otherProductName+" · 2542 · "+date ||
		fake.requests[2].Pipeline != fake.requests[0].Pipeline || fake.requests[2].Stage != fake.requests[0].Stage {
		t.Fatalf("different widgets must use the same destination: %+v", fake.requests)
	}
	if fake.requests[2].ClientAccountID != 2542 || fake.requests[2].ActiveUsers == nil || *fake.requests[2].ActiveUsers != 2 ||
		len(observedAccounts) != 1 || observedAccounts[0] != 2542 {
		t.Fatalf("second account was not observed before deal creation: %+v", fake.requests[2])
	}

	write(wire.Uninstalled, 2, 1)
	write(wire.Reinstalled, 3, 2)
	_, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &contact})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old activation scope was accepted: %v", err)
	}
	mustManager(t, owner.QueryRow(ctx, `
		SELECT id FROM platform_manager.installations WHERE amo_account_id=$1 AND lifecycle='current'`, scope.Account).Scan(&episode))
	_, err = owner.Exec(ctx, `
		UPDATE platform_manager.account_rosters SET failure_code='source_unavailable'
		WHERE account_id=(SELECT account_id FROM platform_manager.installations WHERE id=$1)`, episode)
	mustManager(t, err)
	run()
	if len(fake.requests) != 4 || !fake.requests[3].Create || fake.requests[3].ClientAccountID != scope.Account ||
		fake.requests[3].ActiveUsers != nil || len(observedAccounts) != 2 || observedAccounts[1] != scope.Account {
		t.Fatalf("unavailable user count prevented deal creation or carried an old count: %+v", fake.requests)
	}
}
