package activitybridge

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"sync"
	"testing"
	"time"
)

type admissionPolicy struct {
	deny        bool
	unavailable bool
	issued      int
}

func (p *admissionPolicy) Issue(_ context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	p.issued++
	if p.unavailable {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Unavailable, "policy down")
	}
	if p.deny {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "revoked")
	}
	return serviceapi.Auth{Token: uuid.NewString()}, nil
}
func (p *admissionPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return serviceapi.Principal{}, nil
}

type acceptingActivity struct {
	serviceapi.Activity
	calls        int
	lost         bool
	down         bool
	hook         func()
	operations   map[string]serviceapi.Operation
	settingsDown bool
}

func (a *acceptingActivity) Settings(context.Context, serviceapi.Auth) (serviceapi.Settings, error) {
	if a.settingsDown {
		return serviceapi.Settings{}, serviceapi.Fail(serviceapi.Unavailable, "settings down")
	}
	return serviceapi.DefaultSettings(), nil
}

func TestSyncRetryKeepsOriginalSnapshotDuringActivityOutage(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	receiver := &acceptingActivity{}
	b := New(pool, &admissionPolicy{}, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Sync(ctx, p, "snapshot", SyncInput{Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	receiver.settingsDown = true
	p.TokenID = uuid.NewString()
	replayed, err := b.Sync(ctx, p, "snapshot", SyncInput{Kind: "sync"})
	if err != nil || replayed.CommandID != first.CommandID || !replayed.Replayed {
		t.Fatalf("stable snapshot retry=%+v %v", replayed, err)
	}
	if _, err := b.Sync(ctx, p, "snapshot", SyncInput{Kind: "disable"}); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed command=%v", err)
	}
}

func TestFirstSyncDuringSettingsOutageIsUnavailableWithoutAdmission(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	b := New(pool, &admissionPolicy{}, &acceptingActivity{settingsDown: true}, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	_, err := b.Sync(ctx, p, "new-sync-while-settings-down", SyncInput{Kind: "sync"})
	if serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("initial sync error=%v", err)
	}
	var receipts, outbox int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_command_receipts WHERE installation_id=$1),(SELECT count(*) FROM activity_command_outbox o JOIN activity_command_receipts r USING(command_id) WHERE r.installation_id=$1)`, p.InstallationID).Scan(&receipts, &outbox); err != nil || receipts != 0 || outbox != 0 {
		t.Fatalf("unaccepted sync created work: receipts=%d outbox=%d err=%v", receipts, outbox, err)
	}
}

func TestExpiredFinalDeliveryAttemptDoesNotSendAgain(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	receiver := &acceptingActivity{}
	b := New(pool, &admissionPolicy{}, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "exhausted", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_command_outbox SET status='delivering',attempts=max_attempts,lease_token=$2,leased_until=now()-interval '1 second' WHERE command_id=$1`, first.CommandID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if worked, err := b.DeliverOne(ctx); err != nil || worked || receiver.calls != 0 {
		t.Fatalf("exhausted retry=%v err=%v calls=%d", worked, err, receiver.calls)
	}
	result, err := b.Operation(ctx, p, first.CommandID)
	if err != nil || result.State != "failed" || result.ErrorCode != "delivery_attempts_exhausted" {
		t.Fatalf("exhausted status=%+v %v", result, err)
	}
}

func TestDeliverySkipsExhaustedBeyondCleanupBatch(t *testing.T) {
	for _, state := range []string{"pending_delivery", "delivering"} {
		t.Run(state, func(t *testing.T) {
			pool, p := bridgeDatabase(t)
			ctx := context.Background()
			receiver := &acceptingActivity{}
			b := New(pool, &admissionPolicy{}, receiver, nil)
			if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
				t.Fatal(err)
			}
			for range 101 {
				p.TokenID = uuid.NewString()
				if _, err := b.Configure(ctx, p, uuid.NewString(), serviceapi.DefaultSettings()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `UPDATE activity_command_outbox SET
 status=$1, attempts=max_attempts, run_after=now()-interval '1 hour',
 lease_token=CASE WHEN $1='delivering' THEN gen_random_uuid() ELSE NULL END,
 leased_until=CASE WHEN $1='delivering' THEN now()-interval '1 minute' ELSE NULL END`, state); err != nil {
				t.Fatal(err)
			}
			p.TokenID = uuid.NewString()
			valid, err := b.Configure(ctx, p, "eligible", serviceapi.DefaultSettings())
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := b.DeliverOne(ctx); err != nil || !worked {
				t.Fatalf("delivery worked=%v err=%v", worked, err)
			}
			if receiver.calls != 1 || receiver.operations[valid.CommandID].CommandID != valid.CommandID {
				t.Fatalf("only eligible command must reach receiver: calls=%d operations=%v", receiver.calls, receiver.operations)
			}
			// A later sweep terminalizes the remaining exhausted row, without
			// changing its attempt count or invoking the receiver again.
			if worked, err := b.DeliverOne(ctx); err != nil || worked {
				t.Fatalf("cleanup worked=%v err=%v", worked, err)
			}
			var failed, exceeded int
			if err := pool.QueryRow(ctx, `SELECT
 count(*) FILTER (WHERE status='failed' AND error_code='delivery_attempts_exhausted'),
 count(*) FILTER (WHERE attempts>max_attempts) FROM activity_command_outbox`).Scan(&failed, &exceeded); err != nil {
				t.Fatal(err)
			}
			if failed != 101 || exceeded != 0 || receiver.calls != 1 {
				t.Fatalf("failed=%d exceeded=%d calls=%d", failed, exceeded, receiver.calls)
			}
		})
	}
}

func TestDeliveryCollectorReportsPendingWithFiniteLabels(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	b := New(pool, &admissionPolicy{}, &acceptingActivity{}, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Configure(ctx, p, "metrics", serviceapi.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(b.Collector())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	up, pending := float64(-1), float64(-1)
	for _, family := range families {
		for _, metric := range family.Metric {
			if family.GetName() == "activity_delivery_metrics_up" {
				up = metric.Gauge.GetValue()
			}
			if family.GetName() == "activity_delivery_commands" {
				for _, label := range metric.Label {
					if label.GetName() != "state" {
						t.Fatalf("unexpected label %s", label.GetName())
					}
					if label.GetValue() == "pending_delivery" {
						pending = metric.Gauge.GetValue()
					}
				}
			}
		}
	}
	if up != 1 || pending != 1 {
		t.Fatalf("metrics up=%v pending=%v", up, pending)
	}
}
func (a *acceptingActivity) Configure(_ context.Context, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	a.calls++
	if a.down {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Unavailable, "receiver down")
	}
	if a.hook != nil {
		a.hook()
	}
	if a.operations == nil {
		a.operations = map[string]serviceapi.Operation{}
	}
	op, ok := a.operations[c.CommandID]
	if !ok {
		op = serviceapi.Operation{ID: c.CommandID, CommandID: c.CommandID, State: "succeeded"}
		a.operations[c.CommandID] = op
	}
	if a.lost {
		a.lost = false
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Unavailable, "response lost after receiver commit")
	}
	return op, nil
}
func (a *acceptingActivity) Operation(_ context.Context, r serviceapi.OperationRequest) (serviceapi.Operation, error) {
	op, ok := a.operations[r.OperationID]
	if !ok {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return op, nil
}

func bridgeDatabase(t *testing.T) (*pgxpool.Pool, widgetauth.Principal) {
	t.Helper()
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	p := widgetauth.Principal{IntegrationID: uuid.New(), InstallationID: uuid.New(), AccountID: 42, UserID: 7, Issuer: "https://tenant.amocrm.ru", TokenID: uuid.NewString(), TokenRetainUntil: time.Now().Add(time.Hour)}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,$4,'https://example.test/callback')`, p.IntegrationID, "activity-"+p.IntegrationID.String(), uuid.NewString(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,$3,'tenant.amocrm.ru','active')`, p.InstallationID, p.IntegrationID, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'activity',true)`, p.IntegrationID); err != nil {
		t.Fatal(err)
	}
	return pool, p
}

func TestDurableAcceptanceLostResponseAndActorIsolation(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	policy := &admissionPolicy{}
	receiver := &acceptingActivity{down: true}
	b := New(pool, policy, receiver, nil)
	if _, err := b.Configure(ctx, p, "same-key", serviceapi.DefaultSettings()); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("default pilot admission=%v", err)
	}
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "same-key", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "pending_delivery" || receiver.calls != 0 {
		t.Fatalf("false delivery success: %+v calls=%d", first, receiver.calls)
	}
	if worked, err := b.DeliverOne(ctx); err != nil || !worked {
		t.Fatalf("deliver unavailable=%v %v", worked, err)
	}
	status, err := b.Operation(ctx, p, first.CommandID)
	if err != nil || status.State != "pending_delivery" {
		t.Fatalf("status=%+v %v", status, err)
	}
	fresh := p
	fresh.TokenID = uuid.NewString()
	replay, err := b.Configure(ctx, fresh, "same-key", serviceapi.DefaultSettings())
	if err != nil || !replay.Replayed || replay.CommandID != first.CommandID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := b.Configure(ctx, p, "same-key", serviceapi.DefaultSettings()); !errors.Is(err, widgetauth.ErrReplay) {
		t.Fatalf("JWT replay=%v", err)
	}
	fresh.TokenID = uuid.NewString()
	changed := serviceapi.DefaultSettings()
	changed.RetentionDays++
	if _, err := b.Configure(ctx, fresh, "same-key", changed); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("changed payload=%v", err)
	}
	var receipts, tokens int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM activity_command_receipts),(SELECT count(*) FROM used_widget_tokens)`).Scan(&receipts, &tokens); err != nil || receipts != 1 || tokens != 2 {
		t.Fatalf("receipts=%d tokens=%d err=%v", receipts, tokens, err)
	}
	receiver.down = false
	receiver.lost = true
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `UPDATE activity_command_outbox SET run_after=now()`); err != nil {
			t.Fatal(err)
		}
		if _, err := b.DeliverOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(receiver.operations) != 1 {
		t.Fatalf("duplicate receiver effect: %+v", receiver.operations)
	}
	status, err = b.Operation(ctx, p, first.CommandID)
	if err != nil || status.State != "succeeded" || status.DeliveryState != "accepted" {
		t.Fatalf("merged status=%+v err=%v", status, err)
	}
	other := p
	other.UserID++
	if _, err := b.Operation(ctx, other, first.CommandID); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("other actor read=%v", err)
	}
	other = p
	other.IntegrationID = uuid.New()
	if _, err := b.Operation(ctx, other, first.CommandID); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("other integration read=%v", err)
	}
	if policy.issued < 5 {
		t.Fatal("delivery did not reissue current policy context")
	}
}

func TestWorkerDoesNotDeliverPastRedeliveryHorizon(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	receiver := &acceptingActivity{}
	b := New(pool, &admissionPolicy{}, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "aged", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_command_receipts SET created_at=now()-interval '8 days' WHERE command_id=$1`, first.CommandID); err != nil {
		t.Fatal(err)
	}
	if worked, err := b.DeliverOne(ctx); err != nil || worked || receiver.calls != 0 {
		t.Fatalf("aged delivery=%v err=%v calls=%d", worked, err, receiver.calls)
	}
	var state, code string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,error_code,attempts FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&state, &code, &attempts); err != nil || state != "expired" || code != ErrorDeliveryExpired {
		t.Fatalf("aged state=%s code=%s attempts=%d err=%v", state, code, attempts, err)
	}
	if err := RetryDelivery(ctx, pool, uuid.MustParse(first.CommandID)); !IsDeliveryExpired(err) {
		t.Fatalf("operator retry of expired command=%v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&state, &attempts); err != nil || state != "expired" || attempts != 0 {
		t.Fatalf("expired retry mutated row state=%s attempts=%d err=%v", state, attempts, err)
	}
	status, err := b.Operation(ctx, p, first.CommandID)
	if err != nil || status.State != "failed" || status.ErrorCode != ErrorDeliveryExpired {
		t.Fatalf("widget expired status=%+v %v", status, err)
	}
}

func TestOperatorRetryRejectsAgedFailedCommand(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	policy := &admissionPolicy{}
	receiver := &acceptingActivity{}
	b := New(pool, policy, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "aged-failed", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	policy.deny = true
	if _, err := b.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_command_receipts SET created_at=now()-interval '8 days' WHERE command_id=$1`, first.CommandID); err != nil {
		t.Fatal(err)
	}
	var attemptsBefore int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&attemptsBefore); err != nil {
		t.Fatal(err)
	}
	if err := RetryDelivery(ctx, pool, uuid.MustParse(first.CommandID)); !IsDeliveryExpired(err) {
		t.Fatalf("aged failed retry=%v", err)
	}
	var state string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&state, &attempts); err != nil || state != "failed" || attempts != attemptsBefore {
		t.Fatalf("aged failed retry mutated state=%s attempts=%d want %d err=%v", state, attempts, attemptsBefore, err)
	}
	listed, err := ListDeliveries(ctx, pool)
	if err != nil || len(listed) != 1 || listed[0].CommandID.String() != first.CommandID || listed[0].Status != "failed" {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	inspected, err := InspectDelivery(ctx, pool, uuid.MustParse(first.CommandID))
	if err != nil || inspected.Status != "failed" || inspected.Attempts != attemptsBefore {
		t.Fatalf("inspect=%+v err=%v", inspected, err)
	}
}

func TestDeliveryRevocationAndAuditedRetry(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	policy := &admissionPolicy{}
	receiver := &acceptingActivity{}
	b := New(pool, policy, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "revoke", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	policy.deny = true
	if _, err := b.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	if receiver.calls != 0 {
		t.Fatal("revoked command reached product")
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT status FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if err := RetryDelivery(ctx, pool, uuid.MustParse(first.CommandID)); err != nil {
		t.Fatal(err)
	}
	policy.deny = false
	if _, err := b.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := receiver.operations[first.CommandID]; !ok {
		t.Fatal("manual retry changed command identity")
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action='activity.delivery.retry'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("retry audit=%d %v", audits, err)
	}
}

func TestDeliveryLeaseFencesStaleSender(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	receiver := &acceptingActivity{}
	b := New(pool, &admissionPolicy{}, receiver, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	first, err := b.Configure(ctx, p, "lease", serviceapi.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	receiver.hook = func() {
		if _, err := pool.Exec(ctx, `UPDATE activity_command_outbox SET lease_token=$2 WHERE command_id=$1`, first.CommandID, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.DeliverOne(ctx); serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("stale sender finalized=%v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT status FROM activity_command_outbox WHERE command_id=$1`, first.CommandID).Scan(&state); err != nil || state != "delivering" {
		t.Fatalf("lease state=%s err=%v", state, err)
	}
}

func TestConcurrentAdmissionHasOneDurableCommand(t *testing.T) {
	pool, p := bridgeDatabase(t)
	ctx := context.Background()
	b := New(pool, &admissionPolicy{}, &acceptingActivity{}, nil)
	if err := SetPilot(ctx, pool, p.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan Receipt, 8)
	errs := make(chan error, 8)
	// Exercise the durable transaction directly; policy was checked before it
	// and its capability/pilot locks are rechecked for every transaction.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fresh := p
			fresh.TokenID = uuid.NewString()
			r, err := b.admit(ctx, fresh, "parallel", "activity", "settings", []byte(`{"initial_days":2,"retention_days":7}`))
			if err != nil {
				errs <- err
			} else {
				results <- r
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	id := ""
	for result := range results {
		if id != "" && id != result.CommandID {
			t.Fatal("parallel duplicate command")
		}
		id = result.CommandID
	}
	var receipts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity_command_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("parallel receipts=%d %v", receipts, err)
	}
}
