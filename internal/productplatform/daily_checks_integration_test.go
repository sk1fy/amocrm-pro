package productplatform

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	pb "github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type dailyFixture struct {
	installations []string
	calls         atomic.Int32
	blocked       atomic.Bool
}

func (f *dailyFixture) ListInstallations(context.Context, *pb.AccountDirectoryRequest, ...grpc.CallOption) (*pb.AccountDirectoryPage, error) {
	page := &pb.AccountDirectoryPage{}
	for _, id := range f.installations {
		page.Entries = append(page.Entries, &pb.AccountDirectoryEntry{InstallationId: id, IntegrationId: id, ProductCode: "daily", Current: true, Authorization: "authorized", ObservationOnly: true, RegisteredAt: timestamppb.Now()})
	}
	return page, nil
}
func (f *dailyFixture) ObserveAccount(_ context.Context, r *pb.ObserveAccountRequest, _ ...grpc.CallOption) (*pb.AccountObservation, error) {
	f.calls.Add(1)
	if !r.ObservationOnly || r.CoreInstallationId == f.installations[0] {
		return nil, status.Error(codes.FailedPrecondition, "reauthorization required")
	}
	if f.blocked.Load() {
		return nil, status.Error(codes.Unavailable, "unavailable")
	}
	result := &pb.AccountObservation{ObservedAt: timestamppb.Now()}
	if r.Component == pb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS {
		result.Result = &pb.AccountObservation_Roster{Roster: &pb.CRMUserRoster{Users: []*pb.ObservedCRMUser{{Id: 1, IsActive: true}}}}
	} else {
		result.Result = &pb.AccountObservation_Identity{Identity: &pb.AccountIdentity{CurrentDomain: "pilot.amocrm.ru", DisplayName: "Pilot", WidgetVersions: map[string]string{f.installations[0]: "2.3.1"}}}
	}
	return result, nil
}

func TestPlatformDailyChecks(t *testing.T) {
	ownerDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN")
	if ownerDSN == "" {
		t.Skip("requires isolated Platform PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pool, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 4)
	mustManager(t, err)
	defer pool.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	_, err = owner.Exec(ctx, `DELETE FROM platform_jobs.jobs WHERE kind='accounts.refresh';
 INSERT INTO platform_lifecycle.products(id,code,display_name,line) VALUES('10000000-0000-4400-8000-000000000001','daily','Daily','PRO');
 INSERT INTO platform_lifecycle.product_trial_revisions(product_id,version,rule) SELECT id,1,'none' FROM platform_lifecycle.products WHERE code='daily';
 UPDATE platform_lifecycle.products SET catalog_version=1,catalog_policy_version=1,links='[]',availability='active' WHERE code='daily';
 INSERT INTO platform_lifecycle.accounts(id,amo_account_id) VALUES('10000000-1111-4400-8000-000000000001',444);
 INSERT INTO platform_manager.observed_core_installations SELECT '10000000-1111-4400-8000-000000000002','10000000-1111-4400-8000-000000000002',a.id,p.id,now(),now() FROM platform_lifecycle.accounts a,platform_lifecycle.products p WHERE a.amo_account_id=444 AND p.code='daily'`)
	mustManager(t, err)
	f := &dailyFixture{installations: []string{"10000000-1111-4400-8000-000000000002", "10000000-1111-4400-8000-000000000003"}}
	observer := NewAccountObserver(pool, f)
	observer.SetDirectory(f)
	cfg := queue.DefaultConfig(2)
	cfg.PollInterval = 10 * time.Millisecond
	worker := queue.NewWorker(queue.NewStore(pool), map[queue.Kind]queue.Handler{AccountRefreshJob: observer.RefreshJob}, cfg, prometheus.NewRegistry())
	stopCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(stopCtx) }()
	defer func() { stop(); <-done }()
	for cycle := 1; cycle <= 2; cycle++ {
		if cycle == 2 {
			// Simulate the next deadline in durable state, then start a fresh scheduler.
			_, err = owner.Exec(ctx, `UPDATE platform_manager.account_refresh_slots SET next_check_at=now()-interval '1 second'`)
			mustManager(t, err)
			observer = NewAccountObserver(pool, f)
			observer.SetDirectory(f)
		}
		for {
			mustManager(t, observer.enroll(ctx))
			var n int
			mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_jobs.jobs WHERE kind='accounts.refresh' AND status='succeeded'`).Scan(&n))
			if n == cycle*3 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(30 * time.Millisecond):
			}
		}
		mustManager(t, observer.enroll(ctx))
	}
	if f.calls.Load() != 8 {
		t.Fatalf("expected one fallback per account component per cycle, calls=%d", f.calls.Load())
	}
	var roster, bindings, terms, episodes int
	mustManager(t, owner.QueryRow(ctx, `SELECT (SELECT active_count FROM platform_manager.account_rosters LIMIT 1),(SELECT count(*) FROM platform_manager.core_inventory),(SELECT count(*) FROM platform_lifecycle.terms),(SELECT count(*) FROM platform_lifecycle.episodes)`).Scan(&roster, &bindings, &terms, &episodes))
	if roster != 1 || bindings != 2 || terms != 0 || episodes != 0 {
		t.Fatalf("users=%d bindings=%d terms=%d episodes=%d", roster, bindings, terms, episodes)
	}
	account := uuid.MustParse("10000000-1111-4400-8000-000000000001")
	store := managerStore{pool: pool}
	page, _, err := store.list(ctx, managerActor{}, installationFilters{Account: &account, Limit: 10}, installationSeek{})
	mustManager(t, err)
	if len(page.Items) != 2 || page.Summary.Total != 2 {
		t.Fatalf("expected two observed installations in the account list: %+v", page)
	}
	for _, item := range page.Items {
		if item.Installation.String() == f.installations[0] {
			if item.WidgetVersion == nil || *item.WidgetVersion != "2.3.1" {
				t.Fatal("observed version absent from the matching installation")
			}
		} else if item.WidgetVersion != nil {
			t.Fatal("version leaked into another installation of the same product")
		}
	}
	for _, review := range []string{"unreviewed", "reviewed"} {
		page, _, err := store.list(ctx, managerActor{}, installationFilters{
			Review: review, Lifecycle: "current", Limit: 10,
		}, installationSeek{})
		mustManager(t, err)
		if len(page.Items) != 0 || page.Summary != (installationSummary{}) ||
			page.QueueCounts == nil || *page.QueueCounts != (installationSummary{}) {
			t.Fatalf("%s queue included installations without confirmed episodes: %+v", review, page)
		}
	}
	f.blocked.Store(true)
	_, _, failure := observer.readAccountComponent(ctx, accountRefreshPayload{Account: account, Component: refreshUsers})
	if !failure.Retry || failure.FailureCode != "source_unavailable" {
		t.Fatalf("temporary failure: %+v", failure)
	}
	_, err = owner.Exec(ctx, `INSERT INTO platform_manager.account_profiles(account_id,classification) VALUES ($1,'Запрет работы')`, account)
	mustManager(t, err)
	before := f.calls.Load()
	_, _, failure = observer.readAccountComponent(ctx, accountRefreshPayload{Account: account, Component: refreshUsers})
	if failure.FailureCode != "account_blocked" || before != f.calls.Load() {
		t.Fatalf("blocked account reached CRM: %+v", failure)
	}
}
