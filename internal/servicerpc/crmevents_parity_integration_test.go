package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
)

// This suite uses the same service-owned DB/advisory lock as the owner suite.
// No Core migrations, tables, or runtime DBs are involved in this fixture.
func crmParityPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CRM_EVENTS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CRM_EVENTS_TEST_DATABASE_URL not set; separate owner test database required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_test") || os.Getenv("TEST_DATABASE_RESET_ALLOWED") != "true" {
		t.Fatal("CRM Events parity reset requires a *_test database and TEST_DATABASE_RESET_ALLOWED=true")
	}
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(context.Background(), `SELECT pg_advisory_lock(39081476391)`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(39081476391)`)
		conn.Release()
	})
	if err = migrations.New(pool, "../../migrations/crmevents").Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `TRUNCATE event_sources CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

type crmParityChecker struct {
	primary, other                      serviceapi.Scope
	disabled, unavailable, adminRevoked atomic.Bool
}

func (c *crmParityChecker) Check(_ context.Context, scope serviceapi.Scope, actor int64, system bool) error {
	if c.unavailable.Load() {
		return serviceapi.Fail(serviceapi.Unavailable, "current policy unavailable")
	}
	if c.disabled.Load() || (scope != c.primary && scope != c.other) || (!system && (actor != 7 || c.adminRevoked.Load())) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "current scope or administrator not admitted")
	}
	return nil
}

type crmParityGateway struct{ at int64 }

func (g crmParityGateway) Events(context.Context, serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "parity-event", CreatedAt: g.at, CreatedBy: 7, Type: "lead_added", EntityID: 42, EntityType: "lead"}}}, nil
}
func (crmParityGateway) Users(context.Context, serviceapi.UsersRequest) (serviceapi.Directory, error) {
	return serviceapi.Directory{}, nil
}

type crmParityFixture struct {
	pool   *pgxpool.Pool
	local  *crmevents.Service
	policy *corepolicy.Service
	check  *crmParityChecker
	key    ed25519.PrivateKey
	auth   serviceapi.Auth
	now    time.Time
}

func newCRMParity(t *testing.T) crmParityFixture {
	t.Helper()
	pool := crmParityPool(t)
	check := &crmParityChecker{primary: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, other: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	cfg := crmevents.DefaultConfig()
	cfg.Window = 24 * time.Hour
	cfg.Now = func() time.Time { return now }
	local := crmevents.New(pool, corepolicy.ForCaller(policy, serviceapi.EventsService), crmParityGateway{at: now.Add(-time.Minute).Unix()}, cfg)
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(context.Background(), serviceapi.IssueRequest{Scope: check.primary, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "owner-parity", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	return crmParityFixture{pool, local, policy, check, key, auth, now}
}
func startCRMParity(t *testing.T, ca certAuthority, impl serviceapi.CRMEvents) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(ca.config(t, serviceapi.EventsService, true), &Endpoints{CRMEvents: impl})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}
func crmParityIssue(t *testing.T, f crmParityFixture, scope serviceapi.Scope, grants []serviceapi.Grant) serviceapi.Auth {
	t.Helper()
	auth, err := corepolicy.ForCaller(f.policy, serviceapi.CoreService).Issue(context.Background(), serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "owner-parity-alternative", Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestCRMEventsOwnPostgresApplyStatusOperationLocalMTLSParity(t *testing.T) {
	f := newCRMParity(t)
	ca := newCA(t)
	address := startCRMParity(t, ca, f.local)
	remote := dialTest(t, ca, address, serviceapi.CoreService).CRMEvents
	ctx := context.Background()
	cmd := serviceapi.Command{Auth: f.auth, CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 7}
	// This is the real receiver commit followed by the same command through mTLS.
	accepted, err := f.local.Apply(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := remote.Apply(ctx, cmd)
	if err != nil || !reflect.DeepEqual(accepted, repeated) {
		t.Fatalf("Apply local=%+v grpc=%+v err=%v", accepted, repeated, err)
	}
	if accepted.ID != cmd.CommandID {
		t.Fatalf("command/operation identity %+v", accepted)
	}
	for name, target := range map[string]serviceapi.CRMEvents{"local": f.local, "grpc": remote} {
		conflict := cmd
		conflict.InitialDays = 2
		if _, err := target.Apply(ctx, conflict); serviceapi.ErrorCode(err) != serviceapi.Conflict {
			t.Fatalf("%s different-payload conflict: %v", name, err)
		}
	}
	assertCRMStatusParity(t, f.local, remote, f.auth)
	assertCRMOperationParity(t, f.local, remote, f.auth, accepted.ID, "accepted")
	for i := 0; i < 2; i++ {
		if worked, err := f.local.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("real owner page%d worked=%t err=%v", i, worked, err)
		}
	}
	assertCRMStatusParity(t, f.local, remote, f.auth)
	completed := assertCRMOperationParity(t, f.local, remote, f.auth, accepted.ID, serviceapi.OperationSucceeded)
	if completed.Processed != 2 || completed.Inserted != 1 || completed.Deduplicated != 1 || completed.Updated != 0 {
		t.Fatalf("counter parity %+v", completed)
	}
	for _, target := range []serviceapi.CRMEvents{f.local, remote} {
		again, err := target.Apply(ctx, cmd)
		if err != nil || !reflect.DeepEqual(completed, again) {
			t.Fatalf("committed replay %+v %v", again, err)
		}
	}
	var persisted string
	if err = f.pool.QueryRow(ctx, `SELECT status FROM event_operations WHERE id=$1`, accepted.ID).Scan(&persisted); err != nil || persisted != "completed" {
		t.Fatalf("owner stored state=%s err=%v", persisted, err)
	}
	var inbox, operations, jobs int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_inbox),(SELECT count(*) FROM event_operations),(SELECT count(*) FROM event_jobs)`).Scan(&inbox, &operations, &jobs); err != nil || inbox != 1 || operations != 1 || jobs != 1 {
		t.Fatalf("duplicate effect inbox=%d operations=%d jobs=%d err=%v", inbox, operations, jobs, err)
	}
}
func assertCRMStatusParity(t *testing.T, local, remote serviceapi.CRMEvents, auth serviceapi.Auth) {
	t.Helper()
	a, err := local.Status(context.Background(), auth)
	if err != nil {
		t.Fatal(err)
	}
	b, err := remote.Status(context.Background(), auth)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("Status local=%+v grpc=%+v err=%v", a, b, err)
	}
}
func assertCRMOperationParity(t *testing.T, local, remote serviceapi.CRMEvents, auth serviceapi.Auth, id, want string) serviceapi.Operation {
	t.Helper()
	req := serviceapi.OperationRequest{Auth: auth, OperationID: id}
	a, err := local.Operation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := remote.Operation(context.Background(), req)
	if err != nil || !reflect.DeepEqual(a, b) || a.State != want {
		t.Fatalf("Operation local=%+v grpc=%+v want=%s err=%v", a, b, want, err)
	}
	return a
}

func TestCRMEventsOwnPostgresCurrentPolicyAndScopeLocalMTLSParity(t *testing.T) {
	f := newCRMParity(t)
	ca := newCA(t)
	remote := dialTest(t, ca, startCRMParity(t, ca, f.local), serviceapi.CoreService).CRMEvents
	op, err := f.local.Apply(context.Background(), serviceapi.Command{Auth: f.auth, CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	wrongGrant := crmParityIssue(t, f, f.check.primary, []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionPanel}})
	expired := signedCRMFixture(t, f, f.check.primary, 7, f.now.Add(-time.Minute), "amocrm-pro-services-v1")
	nonAdmin := signedCRMFixture(t, f, f.check.primary, 8, f.now, "amocrm-pro-services-v1")
	wrongAudience := signedCRMFixture(t, f, f.check.primary, 7, f.now, "another-platform")
	for _, tc := range []struct {
		name   string
		auth   serviceapi.Auth
		code   serviceapi.Code
		change func(bool)
	}{
		{name: "forged", auth: serviceapi.Auth{Token: "forged"}, code: serviceapi.Unauthenticated},
		{name: "expired", auth: expired, code: serviceapi.Unauthenticated},
		{name: "wrong token audience", auth: wrongAudience, code: serviceapi.Unauthenticated},
		{name: "wrong action grant", auth: wrongGrant, code: serviceapi.PermissionDenied},
		{name: "non-admin", auth: nonAdmin, code: serviceapi.PermissionDenied},
		{name: "admin revoked", auth: f.auth, code: serviceapi.PermissionDenied, change: f.check.adminRevoked.Store},
		{name: "pilot disabled", auth: f.auth, code: serviceapi.PermissionDenied, change: f.check.disabled.Store},
		{name: "policy unavailable", auth: f.auth, code: serviceapi.Unavailable, change: f.check.unavailable.Store},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.change != nil {
				tc.change(true)
				defer tc.change(false)
			}
			for name, target := range map[string]serviceapi.CRMEvents{"local": f.local, "grpc": remote} {
				_, aerr := target.Apply(context.Background(), serviceapi.Command{Auth: tc.auth, CommandID: uuid.NewString(), Kind: "sync"})
				_, serr := target.Status(context.Background(), tc.auth)
				_, oerr := target.Operation(context.Background(), serviceapi.OperationRequest{Auth: tc.auth, OperationID: op.ID})
				for action, err := range map[string]error{"Apply": aerr, "Status": serr, "Operation": oerr} {
					if serviceapi.ErrorCode(err) != tc.code {
						t.Fatalf("%s %s code=%s err=%v", name, action, serviceapi.ErrorCode(err), err)
					}
				}
			}
		})
	}
	other := crmParityIssue(t, f, f.check.other, serviceapi.UserGrants())
	for name, target := range map[string]serviceapi.CRMEvents{"local": f.local, "grpc": remote} {
		if _, err := target.Operation(context.Background(), serviceapi.OperationRequest{Auth: other, OperationID: op.ID}); serviceapi.ErrorCode(err) != serviceapi.NotFound {
			t.Fatalf("%s foreign operation: %v", name, err)
		}
		status, err := target.Status(context.Background(), other)
		if err != nil || status.Enabled || status.VerifiedThrough != 0 || status.State != "not_enabled" {
			t.Fatalf("%s foreign source status: %+v %v", name, status, err)
		}
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM event_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected requests persisted effects: %d %v", count, err)
	}
}

// The trusted test key signs adversarial contexts; live policy must still reject
// stale/foreign authority instead of trusting a cryptographic signature alone.
func signedCRMFixture(t *testing.T, f crmParityFixture, scope serviceapi.Scope, actor int64, issued time.Time, audience string) serviceapi.Auth {
	t.Helper()
	claims := jwt.MapClaims{"iss": "amocrm-pro-core", "aud": []string{audience}, "iat": issued.Unix(), "nbf": issued.Unix(), "exp": issued.Add(corepolicy.TokenLifetime).Unix(), "scope": scope, "actor_id": actor, "system": false, "consumer": serviceapi.ActivityService, "request_id": "signed-test-context", "grants": serviceapi.UserGrants()}
	token, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return serviceapi.Auth{Token: token}
}

type commitBlockedCRM struct {
	serviceapi.CRMEvents
	dropID    string
	dropped   atomic.Bool
	committed chan serviceapi.Operation
}

func (s *commitBlockedCRM) Apply(ctx context.Context, cmd serviceapi.Command) (serviceapi.Operation, error) {
	op, err := s.CRMEvents.Apply(ctx, cmd)
	if err != nil {
		return op, err
	}
	if cmd.CommandID == s.dropID && s.dropped.CompareAndSwap(false, true) {
		s.committed <- op
		<-ctx.Done()
		return serviceapi.Operation{}, ctx.Err()
	}
	return op, nil
}
func TestCRMEventsOwnPostgresReplayAfterMTLSConnectionLostAfterCommit(t *testing.T) {
	f := newCRMParity(t)
	ca := newCA(t)
	id := uuid.NewString()
	receiver := &commitBlockedCRM{CRMEvents: f.local, dropID: id, committed: make(chan serviceapi.Operation, 1)}
	address := startCRMParity(t, ca, receiver)
	connection := dialTest(t, ca, address, serviceapi.CoreService)
	command := serviceapi.Command{Auth: f.auth, CommandID: id, Kind: "sync", InitialDays: 1}
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _, err := connection.CRMEvents.Apply(ctx, command); result <- err }()
	var committed serviceapi.Operation
	select {
	case committed = <-receiver.committed:
	case err := <-result:
		t.Fatalf("request ended before receiver commit: %v", err)
	case <-ctx.Done():
		t.Fatal("receiver did not commit")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	} // Close the real HTTP/2/mTLS transport while the reply is withheld.
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("lost transport falsely reported command success")
		}
	case <-ctx.Done():
		t.Fatal("client did not observe transport loss")
	}
	reconnect := dialTest(t, ca, address, serviceapi.CoreService).CRMEvents
	replay, err := reconnect.Apply(context.Background(), command)
	if err != nil || !reflect.DeepEqual(replay, committed) {
		t.Fatalf("reconnect replay=%+v committed=%+v err=%v", replay, committed, err)
	}
	assertCRMOperationParity(t, f.local, reconnect, f.auth, id, "accepted")
	var inbox, operations, jobs int
	if err = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM event_inbox),(SELECT count(*) FROM event_operations),(SELECT count(*) FROM event_jobs)`).Scan(&inbox, &operations, &jobs); err != nil || inbox != 1 || operations != 1 || jobs != 1 {
		t.Fatalf("transport retry duplicated effect: inbox=%d operations=%d jobs=%d err=%v", inbox, operations, jobs, err)
	}
}
