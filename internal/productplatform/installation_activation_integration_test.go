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
)

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
	fake := &installationDealStub{}
	service := InstallationDeals{Pool: pool, Source: fake}
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
		fake.requests[0].Title != fake.requests[1].Title || fake.requests[0].Pipeline != 100 || fake.requests[0].Stage != 101 {
		t.Fatalf("uncertain request was not reconciled by its original title: %+v", fake.requests)
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
	otherScope := wire.ActivationScope{Installation: pilot.otherInstallation, Integration: pilot.otherIntegration,
		Account: 2542, Generation: 1, AuthorizationSequence: 1}
	_, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: otherScope, Contact: &contact})
	mustManager(t, err)
	var otherEpisode uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `
		SELECT d.episode_id FROM platform_manager.installation_deals d
		JOIN platform_manager.installations i ON i.id=d.episode_id
		WHERE i.amo_account_id=2542`).Scan(&otherEpisode))
	episode = otherEpisode
	run()
	if len(fake.requests) != 3 || !fake.requests[2].Create ||
		fake.requests[2].Pipeline != fake.requests[0].Pipeline || fake.requests[2].Stage != fake.requests[0].Stage {
		t.Fatalf("different widgets must use the same destination: %+v", fake.requests)
	}

	write(wire.Uninstalled, 2, 1)
	write(wire.Reinstalled, 3, 2)
	_, err = lifecycle.storeActivation(ctx, wire.ActivationRequest{Scope: scope, Contact: &contact})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old activation scope was accepted: %v", err)
	}
}
