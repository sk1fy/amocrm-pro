package productplatform

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestAccountTariffClassificationTransitions(t *testing.T) {
	ownerDSN, jobsDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN"), os.Getenv("PLATFORM_TEST_JOBS_DSN")
	if ownerDSN == "" || jobsDSN == "" {
		t.Skip("requires a disposable migrated Platform database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 1)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, jobsDSN, storage.Jobs, 1)
	mustManager(t, err)
	defer jobs.Close()
	mustManager(t, storage.CheckManager(ctx, jobs))
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	store := NewLifecycleStore(jobs)
	manager := managerStore{pool: jobs}
	scope := wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration,
		Account: 9007199254740993, Generation: 1, AuthorizationSequence: 1}
	base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	installed, err := json.Marshal(wire.Event{
		Version: 1, ID: uuid.New(), Kind: wire.Installed, Installation: scope.Installation, Integration: scope.Integration,
		Account: "9007199254740993", Sequence: 1, Generation: 1, OccurredAt: base,
		Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"},
	})
	mustManager(t, err)
	_, err = store.Record(ctx, installed)
	mustManager(t, err)
	var account, episode uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id,id FROM platform_manager.installations WHERE installation_id=$1`, scope.Installation).Scan(&account, &episode))
	actor := uuid.New()
	command := func(kind string, version int64) managerCommand {
		return managerCommand{ID: uuid.New(), Key: uuid.New(), Subject: actor, RequestID: uuid.New(), Account: account, Kind: kind, Version: version}
	}
	profile := func(cmd managerCommand, raw string, status int) {
		t.Helper()
		patch, err := parseProfilePatch([]byte(raw))
		mustManager(t, err)
		outcome, err := manager.mutate(ctx, cmd, patch, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
			return updateAccountProfile(ctx, tx, cmd, patch)
		})
		mustManager(t, err)
		if outcome.status != status {
			t.Fatalf("profile status = %d, want %d: %s", outcome.status, status, outcome.body)
		}
	}
	subscribe := func(action string, version int64, year int) {
		t.Helper()
		cmd := command(subscriptionCommandKind, version)
		input := subscriptionInput{Action: action, Plan: "top", Seats: 2, Deal: "123", EndsOn: base.AddDate(year, 0, 0).Format(time.DateOnly)}
		outcome, err := manager.mutate(ctx, cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
			return applySubscription(ctx, tx, cmd, input)
		})
		mustManager(t, err)
		if outcome.status != http.StatusOK {
			t.Fatalf("subscription status = %d: %s", outcome.status, outcome.body)
		}
	}
	observe := func(offset time.Duration, tariffID int64) {
		t.Helper()
		mustManager(t, store.RecordAccountLicense(ctx, wire.AccountLicenseRequest{Scope: scope,
			ReceivedAt: base.Add(offset), Hints: wire.AccountLicenseHints{TariffID: &tariffID}}))
	}
	check := func(classification, partner, source, reason string, version int64, technical bool) {
		t.Helper()
		raw, gotVersion, err := readAccountDetail(ctx, jobs, account, []accountCapability{})
		mustManager(t, err)
		var profile struct {
			Classification string
			Partner        string                          `json:"partner_name"`
			Technical      bool                            `json:"technical_tariff"`
			Change         struct{ Source, Reason string } `json:"classification_change"`
		}
		mustManager(t, json.Unmarshal(raw, &profile))
		if gotVersion != version || profile.Classification != classification || profile.Partner != partner ||
			profile.Technical != technical || profile.Change.Source != source || profile.Change.Reason != reason {
			t.Fatalf("unexpected profile at version %d, want %d: %s", gotVersion, version, raw)
		}
	}
	const blocked, partner, free = "Запрет работы", "Клиент партнёра", "Свободный клиент"
	const auto, detected, restored = "widget_tariff", "technical_tariff_detected", "ordinary_tariff_detected"
	profile(command(profileCommandKind, 1), `{"classification":"Клиент партнёра","partner_name":"Fixture Partner","reason":"known partner"}`, http.StatusOK)
	observe(time.Second, technicalAccountTariffID)
	check(blocked, "", auto, detected, 3, true)
	observe(3*time.Second, technicalAccountTariffID)
	observe(2*time.Second, 100)
	check(blocked, "", auto, detected, 3, true)

	subscribe("grant", 3, 1)
	profile(command(profileCommandKind, 4), `{"note":"Fixture note","reason":"note update"}`, http.StatusOK)
	check(blocked, "", auto, detected, 5, true)
	observe(4*time.Second, 100)
	check(partner, "Fixture Partner", auto, restored, 6, false)

	observe(5*time.Second, technicalAccountTariffID)
	check(blocked, "", auto, detected, 7, true)
	const freePatch = `{"classification":"Свободный клиент","reason":"manual review"}`
	profile(command(profileCommandKind, 7), freePatch, http.StatusOK)
	observe(6*time.Second, technicalAccountTariffID)
	subscribe("renew", 8, 2)
	check(free, "", "manual", "", 9, true)
	observe(7*time.Second, 100)
	check(free, "", "manual", "", 9, false)

	observe(8*time.Second, technicalAccountTariffID)
	check(blocked, "", auto, detected, 10, true)
	confirmed := command(profileCommandKind, 10)
	const banPatch = `{"classification":"Запрет работы","reason":"confirm manually"}`
	profile(confirmed, banPatch, http.StatusOK)
	profile(confirmed, banPatch, http.StatusOK)
	profile(command(profileCommandKind, 10), freePatch, http.StatusConflict)
	observe(9*time.Second, 100)
	check(blocked, "", "manual", "", 11, false)
	subscribe("renew", 11, 3)
	check(blocked, "", "manual", "", 12, false)
	observe(10*time.Second, technicalAccountTariffID)
	check(blocked, "", "manual", "", 12, true)
	observe(11*time.Second, 100)
	check(blocked, "", "manual", "", 12, false)

	var changes int
	var evidenceOK, claimOK, stillSkipped bool
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*), bool_or(version=3 AND tariff_id=$2 AND installation_id=$3
		AND generation=1 AND authorization_sequence=1 AND occurred_at=$4 AND reason='technical_tariff_detected'
		AND previous_partner_name='Fixture Partner')
		FROM platform_manager.account_tariff_classification_changes WHERE account_id=$1`, account, technicalAccountTariffID,
		scope.Installation, base.Add(time.Second)).Scan(&changes, &evidenceOK))
	mustManager(t, owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.account_change_events WHERE command_id=$1
		AND actor_id=$2 AND reason='confirm manually' AND details->'classification_confirmed'='true'::jsonb)`, confirmed.ID, actor).Scan(&claimOK))
	mustManager(t, owner.QueryRow(ctx, `SELECT status='skipped' AND reason=$2 FROM platform_manager.installation_deals WHERE episode_id=$1`, episode, dealTechnicalAccount).Scan(&stillSkipped))
	if changes != 4 || !evidenceOK || !claimOK || !stillSkipped {
		t.Fatalf("tariff audit or deal changed: changes=%d evidence=%t manual=%t skipped=%t", changes, evidenceOK, claimOK, stillSkipped)
	}

	profile(command(profileCommandKind, 12), freePatch, http.StatusOK)
	observe(12*time.Second, technicalAccountTariffID)
	check(blocked, "", auto, detected, 14, true)
	// The previous Platform binary writes the classification but has no marker field.
	_, err = jobs.Exec(ctx, `UPDATE platform_manager.account_profiles SET classification='Наш клиент',partner_name=NULL,version=version+1
		WHERE account_id=$1`, account)
	mustManager(t, err)
	observe(13*time.Second, 100)
	var classification string
	var noAutomaticBlock bool
	mustManager(t, owner.QueryRow(ctx, `SELECT classification,technical_classification_version IS NULL FROM platform_manager.account_profiles
		WHERE account_id=$1`, account).Scan(&classification, &noAutomaticBlock))
	if classification != "Наш клиент" || !noAutomaticBlock {
		t.Fatalf("ordinary tariff replaced previous binary's edit: %q, marker cleared=%t", classification, noAutomaticBlock)
	}
}
