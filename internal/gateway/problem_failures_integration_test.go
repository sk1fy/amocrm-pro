package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestProblemFailureClassification(t *testing.T) {
	for _, code := range []serviceapi.Code{serviceapi.PermissionDenied, serviceapi.NotFound, serviceapi.Unauthenticated, serviceapi.ReauthRequired, serviceapi.InvalidArgument} {
		if !terminalProblemFailure(serviceapi.Fail(code, "fixture"), code) {
			t.Fatalf("terminal code retried: %s", code)
		}
	}
	for _, kind := range []amocrm.ErrorKind{amocrm.ErrorUnauthorized, amocrm.ErrorForbidden, amocrm.ErrorPayment, amocrm.ErrorNotFound, amocrm.ErrorValidation, amocrm.ErrorPermanent} {
		err := fmt.Errorf("wrapped: %w", &amocrm.APIError{Kind: kind})
		if !terminalProblemFailure(err, serviceapi.ErrorCode(corepolicy.MapUpstreamError(err))) {
			t.Fatalf("terminal upstream retried: %s", kind)
		}
	}
	for _, kind := range []amocrm.ErrorKind{amocrm.ErrorRateLimited, amocrm.ErrorOverloaded, amocrm.ErrorTemporary} {
		err := &amocrm.APIError{Kind: kind}
		if terminalProblemFailure(err, serviceapi.ErrorCode(corepolicy.MapUpstreamError(err))) {
			t.Fatalf("temporary upstream stopped: %s", kind)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, serviceapi.Fail(serviceapi.Unavailable, "fixture")} {
		if terminalProblemFailure(err, serviceapi.ErrorCode(err)) {
			t.Fatalf("temporary cancellation stopped: %v", err)
		}
	}
}

func TestPermanentProblemFailureRequiresDemandAfterCooldown(t *testing.T) {
	pool := testkit.Postgres(t)
	ctx := context.Background()
	if err := migrations.New(pool, "../../migrations").Up(ctx); err != nil {
		t.Fatal(err)
	}
	testkit.Reset(t, pool)
	cases := []struct {
		name string
		err  error
		code serviceapi.Code
	}{
		{"permission", serviceapi.Fail(serviceapi.PermissionDenied, "disabled"), serviceapi.PermissionDenied},
		{"not-found", serviceapi.Fail(serviceapi.NotFound, "missing"), serviceapi.NotFound},
		{"unauthenticated", serviceapi.Fail(serviceapi.Unauthenticated, "invalid"), serviceapi.Unauthenticated},
		{"reauth", &amocrm.APIError{Kind: amocrm.ErrorUnauthorized, StatusCode: 401}, serviceapi.ReauthRequired},
		{"payment", &amocrm.APIError{Kind: amocrm.ErrorPayment, StatusCode: 402}, serviceapi.PermissionDenied},
		{"permanent", &amocrm.APIError{Kind: amocrm.ErrorPermanent, StatusCode: 418}, serviceapi.Unavailable},
	}
	for _, published := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/published=%t", tc.name, published), func(t *testing.T) {
				scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
				if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'fixture','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
					t.Fatal(err)
				}
				store := &ProblemStore{pool: pool}
				request := func() {
					t.Helper()
					if err := store.Request(ctx, scope); err != nil {
						t.Fatal(err)
					}
				}
				claim := func() problemClaim {
					t.Helper()
					c, err := store.claim(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if c.scope.InstallationID != scope.InstallationID {
						t.Fatalf("unexpected collecting source %+v", c)
					}
					return c
				}
				apply := func(c problemClaim, page amocrm.ProblemPage) {
					t.Helper()
					if err := store.apply(ctx, c, page); err != nil {
						t.Fatal(err)
					}
				}
				request()
				if published {
					apply(claim(), amocrm.ProblemPage{Leads: []amocrm.ProblemLead{{ID: 1, ResponsibleUserID: 7, StatusID: 11}}})
					apply(claim(), amocrm.ProblemPage{})
					if _, err := pool.Exec(ctx, `UPDATE activity_problem_sources SET snapshot_at=now()-interval '8 minutes',next_poll_at=now() WHERE installation_id=$1`, scope.InstallationID); err != nil {
						t.Fatal(err)
					}
					request()
				}
				// Unpublished staging rows must never replace old counts, or become initial zero.
				apply(claim(), amocrm.ProblemPage{Leads: []amocrm.ProblemLead{{ID: 2, ResponsibleUserID: 7, StatusID: 11}}, HasNext: true})
				c := claim()
				failedCtx, cancel := context.WithCancel(ctx)
				cancel()
				failedAt := time.Now()
				store.fail(failedCtx, c, tc.err) // Even expired/cancelled page contexts persist the stop.
				got, err := store.Read(ctx, scope, serviceapi.ProblemRequest{EmployeeIDs: []int64{7}})
				if err != nil || got.Collecting || got.State != "idle" || got.ErrorCode != string(tc.code) || !got.Stale || got.RefreshAfter < failedAt.Add(ProblemRefreshInterval).Unix() {
					t.Fatalf("terminal state %+v %v", got, err)
				}
				if published {
					if got.SnapshotAt == 0 || len(got.Counts) != 1 || got.Counts[0].LeadsNoTasks != 1 {
						t.Fatalf("lost old complete publication: %+v", got)
					}
				} else if got.SnapshotAt != 0 || len(got.Counts) != 0 {
					t.Fatalf("published partial/false-zero result: %+v", got)
				}
				concurrentDemand := func() {
					t.Helper()
					var wg sync.WaitGroup
					errs := make(chan error, 20)
					for i := 0; i < 20; i++ {
						wg.Add(1)
						go func() {
							defer wg.Done()
							if err := store.Request(ctx, scope); err != nil {
								errs <- err
							}
						}()
					}
					wg.Wait()
					close(errs)
					for err := range errs {
						t.Fatal(err)
					}
				}
				concurrentDemand()
				if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("terminal generation auto-claimed: %v", err)
				}
				var generation uuid.UUID
				if err = pool.QueryRow(ctx, `SELECT staging_generation FROM activity_problem_sources WHERE installation_id=$1`, scope.InstallationID).Scan(&generation); err != nil {
					t.Fatal(err)
				}
				if generation != c.generation {
					t.Fatal("requests bypassed cooldown")
				}
				if _, err = pool.Exec(ctx, `UPDATE activity_problem_sources SET next_poll_at=now()-interval '1 second' WHERE installation_id=$1`, scope.InstallationID); err != nil {
					t.Fatal(err)
				}
				if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("cooldown expiry self-retried without demand: %v", err)
				}
				// New demand after the cooldown starts exactly one fresh generation.
				concurrentDemand()
				recovered := claim()
				if recovered.generation == c.generation || recovered.phase != "leads" || recovered.page != 1 {
					t.Fatalf("recovery did not reset failed staging: %+v", recovered)
				}
				if _, err = store.claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("tabs duplicated recovery scan: %v", err)
				}
				apply(recovered, amocrm.ProblemPage{Leads: []amocrm.ProblemLead{{ID: 3, ResponsibleUserID: 7, StatusID: 11}, {ID: 4, ResponsibleUserID: 7, StatusID: 11}, {ID: 5, ResponsibleUserID: 7, StatusID: 11}}})
				apply(claim(), amocrm.ProblemPage{})
				got, err = store.Read(ctx, scope, serviceapi.ProblemRequest{EmployeeIDs: []int64{7}})
				if err != nil || got.Collecting || got.ErrorCode != "" || got.Counts[0].LeadsNoTasks != 3 {
					t.Fatalf("recovery failed: %+v %v", got, err)
				}
			})
		}
	}
}
