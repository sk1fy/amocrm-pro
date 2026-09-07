package crmevents

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func seedRetainedEvents(t *testing.T, store *Postgres, id uuid.UUID, count int) time.Time {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second).Add(-10 * 24 * time.Hour)
	_, err := store.pool.Exec(context.Background(), `INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,content_hash) SELECT $1,'old-'||n,$2::timestamptz+n*interval '1 second',7,'test',1,'lead','hash'::bytea FROM generate_series(1,$3::int) n`, id, at, count)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestRetentionPartialBatchFairnessEmptyAndAtomicRollback(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	store.cfg.RetentionBatch = 2
	first := p.principal.InstallationID
	second := uuid.New()
	if _, err := store.pool.Exec(context.Background(), `INSERT INTO event_sources(installation_id,integration_id) VALUES($1,$2)`, second, p.principal.IntegrationID); err != nil {
		t.Fatal(err)
	}
	at := seedRetainedEvents(t, store, first, 5)
	seedRetainedEvents(t, store, second, 1)
	// Deterministic first turn; the second source is still newer than NULL.
	if _, err := store.pool.Exec(context.Background(), `UPDATE event_sources SET retention_checked_at=now()-interval '1 day' WHERE installation_id=$1`, second); err != nil {
		t.Fatal(err)
	}
	n, err := store.Retain(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("batch %d %v", n, err)
	}
	var frontier time.Time
	if err = store.pool.QueryRow(context.Background(), `SELECT retained_from FROM event_sources WHERE installation_id=$1`, first).Scan(&frontier); err != nil {
		t.Fatal(err)
	}
	if !frontier.Equal(at.Add(3 * time.Second)) {
		t.Fatalf("partial frontier %v expected %v", frontier, at.Add(3*time.Second))
	}
	n, err = store.Retain(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("fair second source batch %d %v", n, err)
	}
	var remaining int
	if err = store.pool.QueryRow(context.Background(), `SELECT count(*) FROM crm_events WHERE installation_id=$1`, first).Scan(&remaining); err != nil || remaining != 3 {
		t.Fatalf("source isolation %d %v", remaining, err)
	}
	// Deliberately fail the frontier update after DELETE: neither effect may commit.
	_, err = store.pool.Exec(context.Background(), `CREATE FUNCTION reject_retention_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.retained_from IS DISTINCT FROM OLD.retained_from THEN RAISE EXCEPTION 'test frontier failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_retention_test BEFORE UPDATE ON event_sources FOR EACH ROW EXECUTE FUNCTION reject_retention_test()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_retention_test ON event_sources; DROP FUNCTION IF EXISTS reject_retention_test()`)
	})
	if _, err = store.Retain(context.Background()); err == nil {
		t.Fatal("injected frontier failure accepted")
	}
	var after time.Time
	if err = store.pool.QueryRow(context.Background(), `SELECT retained_from,(SELECT count(*) FROM crm_events WHERE installation_id=$1) FROM event_sources WHERE installation_id=$1`, first).Scan(&after, &remaining); err != nil || remaining != 3 || !after.Equal(frontier) {
		t.Fatalf("non-atomic cleanup frontier=%v remaining=%d err=%v", after, remaining, err)
	}
	if _, err = store.pool.Exec(context.Background(), `DROP TRIGGER reject_retention_test ON event_sources; DROP FUNCTION reject_retention_test()`); err != nil {
		t.Fatal(err)
	}
	// Drain both turns; the empty source has no verified coverage but must record
	// the retention loss floor for a later initial scan without inventing coverage.
	for i := 0; i < 5; i++ {
		if _, err = store.Retain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	status, err := store.Status(context.Background(), p.principal)
	if err != nil || status.VerifiedFrom != 0 || status.VerifiedThrough != 0 || status.HistoryFrom < time.Now().Add(-8*24*time.Hour).Unix() {
		t.Fatalf("empty/unverified frontier %+v %v", status, err)
	}
	if err = store.pool.QueryRow(context.Background(), `SELECT count(*) FROM crm_events`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining %d %v", remaining, err)
	}
}

func TestRetentionSkipsBusySourceAndNeverRewindsFrontier(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	other := uuid.New()
	if _, err := store.pool.Exec(context.Background(), `INSERT INTO event_sources(installation_id,integration_id) VALUES($1,$2)`, other, p.principal.IntegrationID); err != nil {
		t.Fatal(err)
	}
	seedRetainedEvents(t, store, p.principal.InstallationID, 1)
	seedRetainedEvents(t, store, other, 1)
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `SELECT 1 FROM event_sources WHERE installation_id=$1 FOR UPDATE`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, e := store.Retain(ctx); e != nil || n != 1 {
		t.Fatalf("blocked by another source n=%d err=%v", n, e)
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Retain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err = store.pool.QueryRow(context.Background(), `SELECT retained_from FROM event_sources WHERE installation_id=$1`, p.principal.InstallationID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = store.pool.Exec(context.Background(), `UPDATE event_sources SET retention_days=30 WHERE installation_id=$1`, p.principal.InstallationID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = store.Retain(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var after time.Time
	if err = store.pool.QueryRow(context.Background(), `SELECT retained_from FROM event_sources WHERE installation_id=$1`, p.principal.InstallationID).Scan(&after); err != nil || !after.Equal(before) {
		t.Fatalf("frontier rewound %v -> %v %v", before, after, err)
	}
}

type retentionReadGate struct {
	started, resume chan struct{}
	once            sync.Once
}
type retentionReadKey struct{}

func (g *retentionReadGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, retentionReadKey{}, strings.Contains(d.SQL, "s.continuous_from,s.continuous_to"))
}
func (g *retentionReadGate) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if yes, _ := ctx.Value(retentionReadKey{}).(bool); yes {
		g.once.Do(func() { close(g.started); <-g.resume })
	}
}
func TestQueryKeepsCoverageAndEventsInOneRetentionSnapshot(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	at := seedRetainedEvents(t, store, p.principal.InstallationID, 1)
	if _, err := store.pool.Exec(context.Background(), `UPDATE event_sources SET continuous_from=$2,continuous_to=now() WHERE installation_id=$1`, p.principal.InstallationID, at); err != nil {
		t.Fatal(err)
	}
	gate := &retentionReadGate{started: make(chan struct{}), resume: make(chan struct{})}
	cfg := store.pool.Config()
	cfg.ConnConfig.Tracer = gate
	readPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer readPool.Close()
	readStore := NewPostgres(readPool, store.cfg)
	done := make(chan error, 1)
	go func() {
		q, e := readStore.Query(context.Background(), serviceapi.Query{From: at.Unix(), To: time.Now().Unix(), Limit: 10}, p.principal)
		if e == nil && (len(q.Events) != 1 || q.Status.HistoryFrom != at.Unix() || len(q.Summaries) != 1) {
			e = fmt.Errorf("mixed snapshots: %+v", q)
		}
		done <- e
	}()
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		close(gate.resume)
		t.Fatal("reader did not establish snapshot")
	}
	n, err := store.Retain(context.Background())
	close(gate.resume)
	if err != nil || n != 1 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	q, err := readStore.Query(context.Background(), serviceapi.Query{From: at.Unix(), To: time.Now().Unix(), Limit: 10}, p.principal)
	if err != nil || len(q.Events) != 0 || q.Status.HistoryFrom <= at.Unix() {
		t.Fatalf("new snapshot %+v %v", q, err)
	}
}

func TestBatchDuplicateIDsPreservesSequentialCounters(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "same", CreatedAt: at, CreatedBy: 1}, {ID: "same", CreatedAt: at, CreatedBy: 2}, {ID: "same", CreatedAt: at, CreatedBy: 2}}}, nil
	}
	runPages(t, s, 1)
	got := opState(t, s, op.ID)
	if got.Processed != 3 || got.Inserted != 1 || got.Updated != 1 || got.Deduplicated != 1 {
		t.Fatalf("counter semantics %+v", got)
	}
	var actor int64
	if err := testStore(s).pool.QueryRow(context.Background(), `SELECT created_by FROM crm_events WHERE event_id='same'`).Scan(&actor); err != nil || actor != 2 {
		t.Fatalf("final mutation %d %v", actor, err)
	}
}

func TestRetentionMigrationRejectsLegacyWithoutSilentClamp(t *testing.T) {
	s, p, _ := setup(t)
	accepted(t, s, p)
	store := testStore(s)
	down, err := os.ReadFile("../../../migrations/crmevents/000002_retention_consistency.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../../../migrations/crmevents/000002_retention_consistency.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), string(down)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), `UPDATE event_sources SET retention_days=90; SAVEPOINT legacy`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), string(up)); err == nil || !strings.Contains(err.Error(), "review legacy") {
		t.Fatalf("legacy migration must explicitly reject: %v", err)
	}
	if _, err = tx.Exec(context.Background(), `ROLLBACK TO SAVEPOINT legacy`); err != nil {
		t.Fatal(err)
	}
	var days int
	if err = tx.QueryRow(context.Background(), `SELECT retention_days FROM event_sources`).Scan(&days); err != nil || days != 90 {
		t.Fatalf("legacy changed %d %v", days, err)
	}
	if _, err = tx.Exec(context.Background(), `UPDATE event_sources SET retention_days=30`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), string(up)); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(context.Background(), `SELECT retention_days FROM event_sources`).Scan(&days); err != nil || days != 30 {
		t.Fatalf("valid legacy changed %d %v", days, err)
	}
	if _, err = tx.Exec(context.Background(), `UPDATE event_sources SET retention_days=31`); err == nil {
		t.Fatal("database accepted 31 days")
	}
}

func TestInvalidPageNewSyncRecoversSameWindowWithoutCoverageGap(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	var first serviceapi.EventPageRequest
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		first = r
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "outside", CreatedAt: r.From - 1}}}, nil
	}
	runPages(t, s, 1)
	failed := opState(t, s, op.ID)
	if failed.State != serviceapi.OperationFailed {
		t.Fatalf("invalid page not failed: %+v", failed)
	}
	before, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || before.VerifiedThrough != 0 {
		t.Fatalf("invalid page certified: %+v %v", before, err)
	}
	resumed, err := s.Apply(context.Background(), serviceapi.Command{CommandID: "recover-invalid", Kind: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	g.events = func(r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		if r.From != first.From || r.To != first.To || r.Page != first.Page {
			t.Fatalf("recovery skipped failed window: before %+v after %+v", first, r)
		}
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "fixed", CreatedAt: r.From + 1}}}, nil
	}
	runPages(t, s, 2)
	if got := opState(t, s, resumed.ID); got.State != serviceapi.OperationSucceeded || got.Inserted != 1 || got.Deduplicated != 1 {
		t.Fatalf("recovery %+v", got)
	}
	after, err := s.Status(context.Background(), serviceapi.Auth{})
	if err != nil || after.VerifiedFrom != first.From || after.VerifiedThrough != first.To {
		t.Fatalf("recovery coverage %+v %v", after, err)
	}
}
