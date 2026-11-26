package productplatform

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/amostart"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	pb "github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type accountIdentityFixture struct {
	pb.CoreAccountObservationsClient
	response *pb.AccountObservation
	err      error
}

func (f accountIdentityFixture) ObserveAccount(context.Context, *pb.ObserveAccountRequest, ...grpc.CallOption) (*pb.AccountObservation, error) {
	return f.response, f.err
}

func TestAmoLicenseTrialUsesProviderTimeAndIndependentPaymentEvidence(t *testing.T) {
	ownerDSN, jobsDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN"), os.Getenv("PLATFORM_TEST_JOBS_DSN")
	if ownerDSN == "" || jobsDSN == "" {
		t.Skip("requires a disposable migrated Platform database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 1)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, jobsDSN, storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	store := NewLifecycleStore(jobs)
	scope := wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration,
		Account: 9007199254740993, Generation: 1, AuthorizationSequence: 1}
	event, err := json.Marshal(wire.Event{Version: 1, ID: uuid.New(), Kind: wire.Installed, Installation: scope.Installation,
		Integration: scope.Integration, Account: "9007199254740993", Sequence: 1, Generation: 1, OccurredAt: pilot.opened,
		Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}})
	mustManager(t, err)
	_, err = store.Record(ctx, event)
	mustManager(t, err)
	var account uuid.UUID
	mustManager(t, jobs.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=$1`, scope.Account).Scan(&account))
	deadline := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	submit := func(at time.Time, hints wire.AccountLicenseHints) {
		t.Helper()
		mustManager(t, store.RecordAccountLicense(ctx, wire.AccountLicenseRequest{Scope: scope, ReceivedAt: at, Hints: hints}))
	}
	type licenseStatus struct {
		State, Source string
		TrialEnd      time.Time `json:"trial_end"`
	}
	assertStatus := func(at time.Time, state, source string, trialEnd time.Time) {
		t.Helper()
		var actual licenseStatus
		mustManager(t, jobs.QueryRow(ctx, `SELECT `+amoLicenseStatusJSON+`
            FROM platform_manager.visible_accounts a `+accountObservationJoins+accountLicenseEvidenceJoins+`
            CROSS JOIN (SELECT $2::timestamptz AS as_of) license_clock `+accountLicenseStatusJoin+`
            WHERE a.account_id=$1`, account, at).Scan(&actual))
		if actual.State != state || actual.Source != source || !actual.TrialEnd.Equal(trialEnd) {
			t.Fatalf("at %s got %+v, want %s from %q with trial end %s", at, actual, state, source, trialEnd)
		}
	}
	observePayment := func(at, until time.Time) {
		t.Helper()
		tx, err := jobs.Begin(ctx)
		mustManager(t, err)
		defer tx.Rollback(ctx)
		outcome, err := publishAmoStart(ctx, tx, account, amoStartObservation{AmoAccount: scope.Account, At: at,
			License: amostart.License{Until: until, Tariff: "Fixture paid tariff"}})
		mustManager(t, err)
		if outcome.FailureCode != "" {
			t.Fatalf("unexpected amoSTART outcome %+v", outcome)
		}
		mustManager(t, tx.Commit(ctx))
	}
	noDate := time.Time{}
	previousDay := deadline.Add(-time.Hour)
	submit(previousDay, wire.AccountLicenseHints{TrialEnd: new(deadline.Unix())})
	assertStatus(deadline, "unknown", "", noDate)
	observePayment(previousDay.Add(time.Minute), deadline.Add(48*time.Hour))
	submit(previousDay.Add(2*time.Minute), wire.AccountLicenseHints{PayType: new("TRIAL")})
	assertStatus(deadline.Add(-time.Microsecond), "trial", "widget", deadline)
	assertStatus(deadline, "expired_pending", "widget", deadline)

	// A fresh response covering only the current trial cannot establish payment.
	extended := deadline.Add(4 * time.Hour)
	submit(deadline.Add(time.Minute), wire.AccountLicenseHints{TrialEnd: new(extended.Unix())})
	observePayment(deadline.Add(2*time.Minute), extended)
	assertStatus(deadline.Add(3*time.Minute), "trial", "widget", extended)
	observePayment(deadline.Add(3*time.Minute), extended.Add(48*time.Hour))
	submit(deadline.Add(4*time.Minute), wire.AccountLicenseHints{PayType: new("trial"), TrialEnd: new(extended.Unix())})
	assertStatus(deadline.Add(4*time.Minute), "paid", "amostart", noDate)
	assertStatus(deadline.Add(25*time.Hour), "expired_pending", "widget", extended)

	// The extension is evidence that an older payment result never covered the latest trial.
	extendedAgain := extended.Add(time.Hour)
	submit(deadline.Add(4*time.Minute+30*time.Second), wire.AccountLicenseHints{TrialEnd: new(extendedAgain.Unix())})
	assertStatus(deadline.Add(4*time.Minute+40*time.Second), "trial", "widget", extendedAgain)
	submit(deadline.Add(4*time.Minute+50*time.Second), wire.AccountLicenseHints{PayType: new("paid")})
	assertStatus(deadline.Add(4*time.Minute+50*time.Second), "unknown", "", noDate)

	submit(deadline.Add(5*time.Minute), wire.AccountLicenseHints{PayType: new("blocked")})
	assertStatus(deadline.Add(6*time.Minute), "unpaid", "widget", noDate)
	submit(deadline.Add(6*time.Minute), wire.AccountLicenseHints{PayType: new("paid")})
	assertStatus(deadline.Add(6*time.Minute), "unknown", "", noDate)
	submit(deadline.Add(7*time.Minute), wire.AccountLicenseHints{PayType: new("trial")})
	assertStatus(deadline.Add(8*time.Minute), "trial", "widget", noDate)

	refreshIdentity := func(gateway accountIdentityFixture, want string) {
		t.Helper()
		tx, err := jobs.Begin(ctx)
		mustManager(t, err)
		defer tx.Rollback(ctx)
		mustManager(t, lockRefreshSlot(ctx, tx, account, refreshLicense))
		payload, err := json.Marshal(accountRefreshPayload{Account: account, Component: refreshLicense})
		mustManager(t, err)
		completion := NewAccountObserver(jobs, gateway).RefreshJob(ctx, queue.Job{Payload: payload})
		outcome, err := completion.Publish(ctx, tx)
		mustManager(t, err)
		if outcome.FailureCode != want {
			t.Fatalf("identity refresh returned %q, want %q", outcome.FailureCode, want)
		}
		mustManager(t, tx.Commit(ctx))
	}
	refreshIdentity(accountIdentityFixture{err: status.Error(codes.FailedPrecondition, "authorization required")}, "source_permission_denied")
	assertStatus(deadline.Add(8*time.Minute), "trial", "widget", noDate)
	observePayment(deadline.Add(8*time.Minute), extended.Add(48*time.Hour))
	refreshIdentity(accountIdentityFixture{response: &pb.AccountObservation{ObservedAt: timestamppb.New(deadline.Add(9 * time.Minute)),
		Result: &pb.AccountObservation_PaymentRequired{PaymentRequired: &pb.AccountPaymentRequired{}}}}, accountPaymentRequired)
	assertStatus(deadline.Add(10*time.Minute), "unpaid", "amocrm", noDate)

	var detail struct {
		License licenseStatus `json:"amo_license_status"`
	}
	raw, _, err := readAccountDetail(ctx, jobs, account, []accountCapability{})
	mustManager(t, err)
	mustManager(t, json.Unmarshal(raw, &detail))
	if detail.License.State != "unpaid" || detail.License.Source != "amocrm" {
		t.Fatalf("account detail omitted payment evidence: %s", raw)
	}
	refreshIdentity(accountIdentityFixture{response: &pb.AccountObservation{ObservedAt: timestamppb.New(deadline.Add(11 * time.Minute)),
		Result: &pb.AccountObservation_Identity{Identity: &pb.AccountIdentity{CurrentDomain: "fixture.amocrm.test", DisplayName: "Fixture"}}}}, "")
	assertStatus(deadline.Add(11*time.Minute), "trial", "widget", noDate)
	submit(deadline.Add(12*time.Minute), wire.AccountLicenseHints{PayType: new("paid")})
	assertStatus(deadline.Add(12*time.Minute), "unknown", "", noDate)

	// A newer lookup returning an expired license does not resolve the report.
	observePayment(deadline.Add(12*time.Minute), deadline.In(pilot.zone).AddDate(0, 0, -1))
	assertStatus(deadline.Add(12*time.Minute), "unknown", "", noDate)

	// The earlier scoped evidence keeps the next confirmation due even after it clears.
	for i := range 2 {
		tx, err := jobs.Begin(ctx)
		mustManager(t, err)
		added, err := enrollAmoStart(ctx, tx)
		if err != nil || added != (i == 0) {
			_ = tx.Rollback(ctx)
			t.Fatalf("enrollment %d added=%t, err=%v", i, added, err)
		}
		mustManager(t, tx.Commit(ctx))
	}
	var queued int
	mustManager(t, jobs.QueryRow(ctx, `SELECT count(*) FROM platform_jobs.jobs
        WHERE kind='accounts.refresh' AND status='queued' AND payload->>'component'='amostart'`).Scan(&queued))
	if queued != 1 {
		t.Fatalf("queued %d amoSTART checks, want one", queued)
	}
	observePayment(deadline.Add(13*time.Minute), extended.Add(48*time.Hour))
	assertStatus(deadline.Add(14*time.Minute), "paid", "amostart", noDate)

	submit(deadline.Add(15*time.Minute), wire.AccountLicenseHints{PayType: new("blocked")})
	assertStatus(deadline.Add(15*time.Minute), "unpaid", "widget", noDate)
	observePayment(deadline.Add(16*time.Minute), extended.Add(48*time.Hour))
	submit(deadline.Add(17*time.Minute), wire.AccountLicenseHints{PayType: new("BLOCKED")})
	assertStatus(deadline.Add(17*time.Minute), "paid", "amostart", noDate)

	submit(deadline.Add(18*time.Minute), wire.AccountLicenseHints{PayType: new("paid")})
	assertStatus(deadline.Add(18*time.Minute), "paid", "amostart", noDate)
	submit(deadline.Add(17*time.Minute+30*time.Second), wire.AccountLicenseHints{PayType: new("trial")})
	assertStatus(deadline.Add(18*time.Minute), "unknown", "", noDate)

	// Both delivery orders retain the same final date and invalidate the earlier payment.
	for i, delayed := range []bool{false, true} {
		start := deadline.Add(time.Duration(20+10*i) * time.Minute)
		original := extended.Add(time.Duration(i+1) * time.Hour)
		alternate := original.Add(time.Hour)
		submit(start, wire.AccountLicenseHints{PayType: new("trial"), TrialEnd: new(original.Unix())})
		observePayment(start.Add(time.Minute), extended.Add(48*time.Hour))
		changedAt, latestAt := start.Add(2*time.Minute), start.Add(3*time.Minute)
		if delayed {
			submit(latestAt, wire.AccountLicenseHints{TrialEnd: new(original.Unix())})
			assertStatus(latestAt, "paid", "amostart", noDate)
			submit(changedAt, wire.AccountLicenseHints{TrialEnd: new(alternate.Unix())})
		} else {
			submit(changedAt, wire.AccountLicenseHints{TrialEnd: new(alternate.Unix())})
			submit(latestAt, wire.AccountLicenseHints{TrialEnd: new(original.Unix())})
		}
		assertStatus(latestAt, "trial", "widget", original)
	}
	submit(deadline.Add(34*time.Minute), wire.AccountLicenseHints{PayType: new("paid")})

	day := deadline.In(pilot.zone).AddDate(0, 0, 2)
	paidDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, pilot.zone)
	nextDay := paidDay.AddDate(0, 0, 1)
	observePayment(paidDay.Add(8*time.Hour), paidDay)
	assertStatus(nextDay.Add(-time.Microsecond), "paid", "amostart", noDate)
	assertStatus(nextDay, "unknown", "", noDate)
}
