package crmevents

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type testPolicy struct {
	principal  serviceapi.Principal
	issueError error
}

func (p *testPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return p.principal, nil
}
func (p *testPolicy) Issue(context.Context, serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return serviceapi.Auth{Token: "system"}, p.issueError
}

type testGateway struct {
	events func(serviceapi.EventPageRequest) (serviceapi.EventPage, error)
	calls  int
}

func (g *testGateway) Events(_ context.Context, r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	g.calls++
	if r.Limit != 100 {
		panic("unverified page limit")
	}
	return g.events(r)
}
func (g *testGateway) Users(context.Context, serviceapi.UsersRequest) (serviceapi.Directory, error) {
	return serviceapi.Directory{}, nil
}

func eventsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CRM_EVENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CRM_EVENTS_TEST_DATABASE_URL not set; separate owner test database required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(config.ConnConfig.Database, "_test") {
		t.Fatal("CRM Events test database must end in _test")
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(context.Background(), `SELECT pg_advisory_lock(39081476391)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(39081476391)`)
		conn.Release()
	})
	if err = migrations.New(pool, "../../../migrations/crmevents").Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `TRUNCATE event_sources CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}
func setup(t *testing.T) (*Service, *testPolicy, *testGateway) {
	pool := eventsPool(t)
	policy := &testPolicy{principal: serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 77, Consumer: "activity"}}
	gateway := &testGateway{events: func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }}
	cfg := DefaultConfig()
	cfg.Now = func() time.Time { return time.Now().UTC().Truncate(time.Second) }
	return New(pool, policy, gateway, cfg), policy, gateway
}
func accepted(t *testing.T, s *Service, p *testPolicy) serviceapi.Operation {
	t.Helper()
	op, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "start", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep focused store tests to one fixed hour rather than 48 initial windows.
	if _, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET window_from=date_trunc('second',now())-interval '1 hour',window_to=date_trunc('second',now()),target_to=date_trunc('second',now()) WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	return op
}
func runPages(t *testing.T, s *Service, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		worked, err := s.RunOnce(context.Background())
		if err != nil || !worked {
			t.Fatalf("page %d worked=%t err=%v", i, worked, err)
		}
	}
}
func opState(t *testing.T, s *Service, id string) serviceapi.Operation {
	t.Helper()
	op, err := s.Operation(context.Background(), serviceapi.OperationRequest{OperationID: id})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestInboxLostReplyConflictAndEmptyWindow(t *testing.T) {
	s, p, _ := setup(t)
	op := accepted(t, s, p)
	replay, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "start", Kind: "sync", Auth: serviceapi.Auth{Token: "reissued"}})
	if err != nil || replay.ID != op.ID {
		t.Fatalf("lost reply replay %+v %v", replay, err)
	}
	_, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "start", Kind: "sync", InitialDays: 3})
	if serviceapi.ErrorCode(err) != serviceapi.Conflict {
		t.Fatalf("conflict %v", err)
	}
	runPages(t, s, 1)
	state, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || state.VerifiedThrough != 0 {
		t.Fatalf("first pass claimed completion: %+v %v", state, err)
	}
	runPages(t, s, 1)
	state, err = s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || state.VerifiedThrough == 0 || state.LastEventAt != 0 {
		t.Fatalf("empty verified range: %+v %v", state, err)
	}
	if opState(t, s, op.ID).State != serviceapi.OperationSucceeded {
		t.Fatal("operation not completed")
	}
}
func TestPartialPageRestartDedupUpdatesAndSameTimestampCursor(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		if r.Page == 1 {
			return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "a", CreatedAt: at, CreatedBy: 7}, {ID: "b", CreatedAt: at, CreatedBy: 7}}, HasNext: true}, nil
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "c", CreatedAt: at, CreatedBy: 8}}}, nil
	}
	runPages(t, s, 1)
	// New business instance shares only its owner DB and narrow ports.
	s = New(testStore(s).pool, p, g, s.cfg)
	runPages(t, s, 3)
	actual := opState(t, s, op.ID)
	if actual.Processed != 6 || actual.Inserted != 3 || actual.Deduplicated != 3 || actual.Updated != 0 {
		t.Fatalf("counters %+v", actual)
	}
	q := serviceapi.Query{From: at - 1, To: at + 1, Limit: 2}
	result, err := s.Query(context.Background(), q)
	if err != nil || len(result.Events) != 2 || result.NextCursor == "" || len(result.Summaries) != 2 {
		t.Fatalf("query %+v %v", result, err)
	}
	q.Cursor = result.NextCursor
	result, err = s.Query(context.Background(), q)
	if err != nil || len(result.Events) != 1 || result.Events[0].ID != "c" {
		t.Fatalf("same timestamp cursor %+v %v", result, err)
	}
	// Another overlapping window changes one existing event without adding a row.
	_, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "again", Kind: "backfill", From: at - 10, To: at + 10})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "a", CreatedAt: at, CreatedBy: 99}}}, nil
	}
	runPages(t, s, 2)
	var count int
	if err = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM crm_events`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("upsert count=%d %v", count, err)
	}
}
func TestLeaseFencingAndDisableInvalidateInflightWriter(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	a, err := testStore(s).Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_sources SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	b, err := testStore(s).Claim(context.Background())
	if err != nil || b.Token <= a.Token {
		t.Fatalf("replacement %+v %v", b, err)
	}
	if err = testStore(s).SavePage(context.Background(), a, serviceapi.EventPage{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale writer accepted: %v", err)
	}
	if _, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "stop", Kind: "disable"}); err != nil {
		t.Fatal(err)
	}
	if err = testStore(s).SavePage(context.Background(), b, serviceapi.EventPage{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("disabled writer accepted: %v", err)
	}
	if worked, err := s.RunOnce(context.Background()); worked || err != nil {
		t.Fatalf("disabled admitted: %t %v", worked, err)
	}
}
func TestMutatingPaginationReplaysMissingEventAndNeverVerifiesUnstable(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	pass := 0
	e := func(id string) serviceapi.Event { return serviceapi.Event{ID: id, CreatedAt: at} }
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		if r.Page == 1 {
			pass++
			if pass == 1 {
				return serviceapi.EventPage{Events: []serviceapi.Event{e("a"), e("b")}, HasNext: true}, nil
			}
			return serviceapi.EventPage{Events: []serviceapi.Event{e("a"), e("c")}, HasNext: true}, nil
		}
		if pass == 1 {
			return serviceapi.EventPage{Events: []serviceapi.Event{e("d")}}, nil
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{e("b"), e("d")}}, nil
	}
	runPages(t, s, 6)
	if opState(t, s, op.ID).State != serviceapi.OperationSucceeded {
		t.Fatal("stabilized replay not complete")
	}
	var count int
	_ = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM crm_events`).Scan(&count)
	if count != 4 {
		t.Fatalf("missed shifted event, count=%d", count)
	}
	// A later window with perpetual changes must retain previous checked progress.
	before, _ := s.Status(context.Background(), serviceapi.Auth{})
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "unstable", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{e(uuid.NewString())}}, nil
	}
	runPages(t, s, 3)
	after, _ := s.Status(context.Background(), serviceapi.Auth{})
	if after.ErrorCode != "pagination_unstable" || after.VerifiedThrough != before.VerifiedThrough {
		t.Fatalf("unstable advanced progress: %+v", after)
	}
}
func TestTwoSchedulersAndWorkersCannotDuplicateSource(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	runPages(t, s, 2)
	_, err := testStore(s).pool.Exec(context.Background(), `UPDATE event_sources SET next_poll_at=now()-interval '1 second'`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Schedule(context.Background()) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count int
	_ = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM event_jobs WHERE status='queued'`).Scan(&count)
	if count != 1 {
		t.Fatalf("scheduled %d duplicate jobs", count)
	}
	_, err = testStore(s).Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testStore(s).Claim(context.Background()); err == nil {
		t.Fatal("two workers leased same source")
	}
}
func TestPolicyUnavailableReauthAndRecoveryRetainCursor(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	p.issueError = serviceapi.Fail(serviceapi.Unavailable, "policy down")
	runPages(t, s, 1)
	if g.calls != 0 {
		t.Fatal("policy outage bypassed")
	}
	_, _ = testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET run_after=now()`)
	p.issueError = nil
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{}, serviceapi.Fail(serviceapi.ReauthRequired, "reauth")
	}
	runPages(t, s, 1)
	state, _ := s.Status(context.Background(), serviceapi.Auth{})
	if !state.ReauthRequired || state.NextPage != 1 {
		t.Fatalf("reauth status %+v", state)
	}
	if worked, err := s.RunOnce(context.Background()); worked || err != nil {
		t.Fatalf("reauth kept running %t %v", worked, err)
	}
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "reauthorized", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }
	runPages(t, s, 2)
	state, _ = s.Status(context.Background(), serviceapi.Auth{})
	if state.ReauthRequired || state.VerifiedThrough == 0 {
		t.Fatalf("resume %+v", state)
	}
}
func TestBackfillIslandDoesNotCloseGapAndRetentionDoesNotRewind(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	runPages(t, s, 2)
	before, _ := s.Status(context.Background(), serviceapi.Auth{})
	now := time.Now().Unix()
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "island", Kind: "backfill", From: now - 5*86400, To: now - 4*86400})
	if err != nil {
		t.Fatal(err)
	}
	// Focus one fixed backfill window and emulate a full processed day.
	_, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET target_to=window_to WHERE kind='backfill'`)
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "old", CreatedAt: r.From + 1}}}, nil
	}
	runPages(t, s, 2)
	after, _ := s.Status(context.Background(), serviceapi.Auth{})
	if after.VerifiedFrom != before.VerifiedFrom || after.VerifiedThrough != before.VerifiedThrough {
		t.Fatalf("island bridged gap: before %+v after %+v", before, after)
	}
	_, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_sources SET retention_days=2`)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.Retain(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("retention removed %d: %v", removed, err)
	}
	after, _ = s.Status(context.Background(), serviceapi.Auth{})
	if after.VerifiedThrough != before.VerifiedThrough {
		t.Fatal("retention rewound sync progress")
	}
}
func TestInstallationIsolationIncludingSameAccountIntegration(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "private", CreatedAt: at}}}, nil
	}
	runPages(t, s, 2)
	// Even a mismatched trusted principal cannot read a source from another integration.
	p.principal.IntegrationID = uuid.New()
	if _, err := s.Operation(context.Background(), serviceapi.OperationRequest{OperationID: op.ID}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("cross-integration operation %v", err)
	}
	result, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1})
	if err != nil || len(result.Events) != 0 {
		t.Fatalf("cross-integration events: %+v %v", result, err)
	}
	p.principal.InstallationID = uuid.New()
	if _, err := s.Operation(context.Background(), serviceapi.OperationRequest{OperationID: op.ID}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("cross-source operation %v", err)
	}
	state, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || state.Enabled || state.VerifiedThrough != 0 {
		t.Fatalf("cross-source status %+v %v", state, err)
	}
}

func TestPageWriteAndContinuationRollbackTogether(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	c, err := testStore(s).Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = testStore(s).pool.Exec(context.Background(), `CREATE OR REPLACE FUNCTION reject_test_continuation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected continuation failure'; END $$; CREATE TRIGGER reject_test_continuation BEFORE UPDATE ON event_jobs FOR EACH ROW EXECUTE FUNCTION reject_test_continuation()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testStore(s).pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_test_continuation ON event_jobs; DROP FUNCTION IF EXISTS reject_test_continuation()`)
	})
	page := serviceapi.EventPage{Events: []serviceapi.Event{{ID: "atomic", CreatedAt: c.From.Unix() + 1}}, HasNext: true}
	if err = testStore(s).SavePage(context.Background(), c, page); err == nil {
		t.Fatal("injected continuation failure was ignored")
	}
	var count, position int
	_ = testStore(s).pool.QueryRow(context.Background(), `SELECT count(*) FROM crm_events`).Scan(&count)
	_ = testStore(s).pool.QueryRow(context.Background(), `SELECT page FROM event_jobs WHERE id=$1`, c.ID).Scan(&position)
	if count != 0 || position != 1 {
		t.Fatalf("partial transaction escaped: events=%d page=%d", count, position)
	}
	_, err = testStore(s).pool.Exec(context.Background(), `DROP TRIGGER reject_test_continuation ON event_jobs; DROP FUNCTION reject_test_continuation()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = testStore(s).SavePage(context.Background(), c, page); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillGlobalBudgetAndCurrentPriority(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	runPages(t, s, 2)
	now := time.Now().Unix()
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "background1", Kind: "backfill", From: now - 7200, To: now - 3600})
	if err != nil {
		t.Fatal(err)
	}
	first, err := testStore(s).Claim(context.Background())
	if err != nil || first.Kind != "backfill" {
		t.Fatalf("first %+v %v", first, err)
	}
	p.principal.Scope = serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	accepted(t, s, p)
	runPages(t, s, 2)
	_, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "background2", Kind: "backfill", From: now - 7200, To: now - 3600})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = testStore(s).Claim(context.Background()); err == nil {
		t.Fatal("second background source exceeded global backfill budget")
	}
	_, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "second", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := testStore(s).Claim(context.Background())
	if err != nil || second.Kind != "current" {
		t.Fatalf("current behind background %+v %v", second, err)
	}
}

func TestGatewayNetworkDoesNotHoldWriteTransactionAndMetrics(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		tx, err := testStore(s).pool.Begin(context.Background())
		if err != nil {
			return serviceapi.EventPage{}, err
		}
		defer tx.Rollback(context.Background())
		_, err = tx.Exec(context.Background(), `SELECT installation_id FROM event_sources WHERE installation_id=$1 FOR UPDATE NOWAIT`, p.principal.InstallationID)
		return serviceapi.EventPage{}, err
	}
	runPages(t, s, 2)
	registry := prometheus.NewRegistry()
	registry.MustRegister(s.Collector())
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range metrics {
		if m.GetName() == "crm_events_metrics_up" {
			found = true
			if m.Metric[0].Gauge.GetValue() != 1 {
				t.Fatal("metrics query failed")
			}
		}
	}
	if !found {
		t.Fatal("no owner metrics")
	}
}

func TestSourceReauthBlocksOtherQueuedJobsAndManualFailurePreservesPolling(t *testing.T) {
	s, p, g := setup(t)
	accepted(t, s, p)
	now := time.Now().Unix()
	_, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "other", Kind: "backfill", From: now - 7200, To: now - 3600})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{}, serviceapi.Fail(serviceapi.ReauthRequired, "expired")
	}
	runPages(t, s, 1)
	if worked, err := s.RunOnce(context.Background()); worked || err != nil {
		t.Fatalf("reauth source admitted another queued job: %t %v", worked, err)
	}
	_, err = s.Apply(context.Background(), serviceapi.Command{CommandID: "resume", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) { return serviceapi.EventPage{}, nil }
	runPages(t, s, 2)
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: uuid.NewString(), CreatedAt: r.From + 1}}}, nil
	}
	runPages(t, s, 3)
	state, _ := s.Status(context.Background(), serviceapi.Auth{})
	if state.State != "idle" || state.ErrorCode != "pagination_unstable" {
		t.Fatalf("manual failure stopped source: %+v", state)
	}
	_, _ = testStore(s).pool.Exec(context.Background(), `UPDATE event_sources SET next_poll_at=now()-interval '1 second'`)
	if err = s.Schedule(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := testStore(s).Claim(context.Background())
	if err != nil || next.Kind != "current" {
		t.Fatalf("current collection not rescheduled %+v %v", next, err)
	}
}

func testStore(s *Service) *Postgres { return s.repository.(*Postgres) }

func TestCoreCommandUUIDIsRecipientOperationID(t *testing.T) {
	s, _, _ := setup(t)
	id := uuid.NewString()
	op, err := s.Apply(context.Background(), serviceapi.Command{CommandID: id, Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	if op.ID != id || op.CommandID != id {
		t.Fatalf("Core command/operation identity mismatch: %+v", op)
	}
	again, err := s.Operation(context.Background(), serviceapi.OperationRequest{OperationID: id})
	if err != nil || again.ID != id {
		t.Fatalf("Core lookup by stable operation id: %+v %v", again, err)
	}
}
func TestMetricsMapUnknownStorageStateAndRejectCompletedLease(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	c, err := testStore(s).Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = testStore(s).SavePage(context.Background(), c, serviceapi.EventPage{}); err != nil {
		t.Fatal(err)
	}
	if err = testStore(s).SavePage(context.Background(), c, serviceapi.EventPage{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("completed page lease reused: %v", err)
	}
	_, err = testStore(s).pool.Exec(context.Background(), `UPDATE event_jobs SET status=$1`, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := testStore(s).MetricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.States) != 1 || snapshot.States["other"] != 1 {
		t.Fatalf("unbounded metric states: %+v", snapshot.States)
	}
}
