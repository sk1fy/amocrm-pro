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
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestAccountTariffScopeFreshnessAndTechnicalDeal(t *testing.T) {
	ownerDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN")
	jobsDSN := os.Getenv("PLATFORM_TEST_JOBS_DSN")
	if ownerDSN == "" || jobsDSN == "" {
		t.Skip("requires a disposable migrated Platform database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 1)
	mustManager(t, err)
	defer owner.Close()
	pool, err := storage.Open(ctx, jobsDSN, storage.Jobs, 2)
	mustManager(t, err)
	defer pool.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)

	store := NewLifecycleStore(pool)
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	emit := func(scope wire.ActivationScope, kind wire.Kind, source string) {
		t.Helper()
		raw, err := json.Marshal(wire.Event{
			Version: 1, ID: uuid.New(), Kind: kind, Installation: scope.Installation, Integration: scope.Integration,
			Account: "9007199254740993", Sequence: scope.AuthorizationSequence, Generation: scope.Generation,
			OccurredAt: base.Add(time.Duration(scope.AuthorizationSequence) * time.Second),
			Evidence:   wire.Evidence{ID: uuid.New(), Source: source},
		})
		mustManager(t, err)
		_, err = store.Record(ctx, raw)
		mustManager(t, err)
	}
	scope := wire.ActivationScope{
		Installation: pilot.installation, Integration: pilot.integration, Account: 9007199254740993, Generation: 1, AuthorizationSequence: 1,
	}
	emit(scope, wire.Installed, "core_oauth")
	var account, episode uuid.UUID
	var openedAt time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id,id,opened_at FROM platform_manager.installations WHERE installation_id=$1`, scope.Installation).
		Scan(&account, &episode, &openedAt))

	fake := &installationDealStub{}
	deals := InstallationDeals{Pool: pool, Source: fake, RequireKnownTariff: true}
	assertDecision := func(expected string) {
		t.Helper()
		_, decision, err := deals.reserve(ctx, episode)
		mustManager(t, err)
		if decision != expected {
			t.Fatalf("deal decision %q, want %q", decision, expected)
		}
	}
	assertDecision(dealTariffUnknown)

	submit := func(from wire.ActivationScope, offset time.Duration, hints wire.AccountLicenseHints) error {
		return store.RecordAccountLicense(ctx, wire.AccountLicenseRequest{Scope: from, ReceivedAt: openedAt.Add(offset), Hints: hints})
	}
	ordinary := int64(100)
	technical := technicalAccountTariffID
	firstEnd := base.Add(time.Hour).Unix()
	mustManager(t, submit(scope, -time.Second, wire.AccountLicenseHints{TariffID: &ordinary}))
	mustManager(t, submit(scope, time.Second, wire.AccountLicenseHints{PayType: new("trial"), TrialEnd: &firstEnd}))
	assertDecision(dealTariffUnknown)

	wrongAccount := scope
	wrongAccount.Account = 2542
	if err := submit(wrongAccount, 2*time.Second, wire.AccountLicenseHints{TariffID: &technical}); !errors.Is(err, ErrEventScope) {
		t.Fatalf("wrong account returned %v", err)
	}
	previousScope := scope
	scope.AuthorizationSequence++
	emit(scope, wire.Reauthorized, "core_oauth")
	if err := submit(previousScope, 2*time.Second, wire.AccountLicenseHints{TariffID: &technical}); !errors.Is(err, ErrEventScope) {
		t.Fatalf("previous authorization returned %v", err)
	}

	mustManager(t, submit(scope, 4*time.Second, wire.AccountLicenseHints{TariffID: &technical}))
	mustManager(t, submit(scope, 5*time.Second, wire.AccountLicenseHints{PayType: new("trial")}))
	mustManager(t, submit(scope, 3*time.Second, wire.AccountLicenseHints{TariffID: &ordinary, PayType: new("blocked")}))
	mustManager(t, submit(scope, 4*time.Second, wire.AccountLicenseHints{TariffID: &ordinary}))
	mustManager(t, submit(scope, 2*time.Second, wire.AccountLicenseHints{TrialEnd: new(firstEnd + 1)}))

	var classification, dealState, reason, jobState, payType string
	var tariff, authSequence, trialEnd int64
	var tariffTime, trialReset time.Time
	mustManager(t, owner.QueryRow(ctx, `
		SELECT p.classification,d.status,d.reason,j.status,t.numeric_value,t.authorization_sequence,t.observed_at,
			trial.numeric_value,pay.trial_reset_at,pay.text_value
		FROM platform_manager.account_profiles p
		JOIN platform_manager.account_tariff_fields t USING(account_id)
		JOIN platform_manager.account_tariff_fields trial USING(account_id)
		JOIN platform_manager.account_tariff_fields pay USING(account_id)
		JOIN platform_manager.installation_deals d ON d.episode_id=$2
		JOIN platform_jobs.jobs j ON j.id=d.episode_id
		WHERE p.account_id=$1 AND t.field='tariff_id' AND trial.field='trial_end' AND pay.field='pay_type'`, account, episode).
		Scan(&classification, &dealState, &reason, &jobState, &tariff, &authSequence, &tariffTime, &trialEnd, &trialReset, &payType))
	if classification != "Запрет работы" || dealState != "skipped" || reason != dealTechnicalAccount || jobState != "succeeded" {
		t.Fatalf("technical classification and deal states: %q, %q, %q, %q", classification, dealState, reason, jobState)
	}
	if tariff != technical || authSequence != scope.AuthorizationSequence || !tariffTime.Equal(openedAt.Add(4*time.Second)) ||
		trialEnd != firstEnd || !trialReset.Equal(openedAt.Add(3*time.Second)) || payType != "trial" {
		t.Fatalf("observation lost per-field freshness: tariff=%d authorization=%d at=%s trial=%d reset=%s pay=%s",
			tariff, authSequence, tariffTime, trialEnd, trialReset, payType)
	}
	completion := deals.Run(ctx, queue.Job{ID: episode, Kind: InstallationDealJob})
	if completion.Outcome.FailureCode != "" || completion.Publish != nil || len(fake.requests) != 0 {
		t.Fatal("skipped technical deal reached the outbound worker")
	}

	mustManager(t, submit(scope, 6*time.Second, wire.AccountLicenseHints{TariffID: &ordinary, TrialEnd: new(firstEnd + 2)}))
	if _, _, err := deals.reserve(ctx, episode); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("later ordinary tariff revived the skipped deal: %v", err)
	}
	previousScope = scope
	scope.AuthorizationSequence++
	emit(scope, wire.Uninstalled, "core_uninstall")
	scope.AuthorizationSequence++
	scope.Generation++
	emit(scope, wire.Reinstalled, "core_oauth")
	if err := submit(previousScope, 7*time.Second, wire.AccountLicenseHints{TariffID: &technical}); !errors.Is(err, ErrEventScope) {
		t.Fatalf("previous episode returned %v", err)
	}
	mustManager(t, submit(scope, 8*time.Second, wire.AccountLicenseHints{TariffID: &ordinary}))
	var count int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_manager.installation_deals d
		JOIN platform_lifecycle.episodes e ON e.id=d.episode_id WHERE e.installation_id=$1`, scope.Installation).Scan(&count))
	if count != 1 {
		t.Fatalf("reauthorization or reinstall scheduled a new deal: %d", count)
	}
}
