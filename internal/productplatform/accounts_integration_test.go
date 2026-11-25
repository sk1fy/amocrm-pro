package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/amostart"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestPlatformAccountsAcrossReplicas(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR",
		"PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(name) == "" {
			t.Skip("run make platform-test for Platform manager process checks")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	_, err = owner.Exec(ctx, `DELETE FROM platform_jobs.jobs WHERE kind='accounts.refresh'`)
	mustManager(t, err)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	gateway := &accountFixtureGateway{calls: make(chan accountFixtureCall, 8)}
	address := startAccountFixtureGateway(t, certs, gateway)
	env := []string{"PLATFORM_ACCOUNT_GATEWAY_ADDRESS=" + address,
		"PLATFORM_ACCOUNT_GATEWAY_IDENTITY_DIR=" + filepath.Join(certs, "platform"),
		"PLATFORM_ACCOUNT_GATEWAY_SERVER_NAME=localhost"}
	first, second := startManagerProcess(t, env...), startManagerProcess(t, env...)
	manager := tlsClient(t, certs, "manager-bff")
	manager.Timeout = 5 * time.Second
	pilot.deliver(t, tlsClient(t, certs, "core"), first.private)

	var account, episode uuid.UUID
	var episodeVersion int64
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id,id,version FROM platform_manager.installations WHERE amo_account_id=2542`).
		Scan(&account, &episode, &episodeVersion))
	refreshPath := managerPath + "accounts/" + account.String() + "/refresh"
	usersPath := managerPath + "accounts/" + account.String() + "/crm-users"

	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	other := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	reader := managerTestActor{subject: writer.subject, version: 2, grants: []string{"clients.read"}}
	cmd1, cmd2 := newManagerTestCommand(account, 0), newManagerTestCommand(account, 0)
	const body = `{"components":["users"],"reason":"Check roster"}`
	path := func(cmd managerTestCommand) string { return managerPath + "operations/" + cmd.id.String() }

	replies := make(chan observedManagerResponse, 2)
	go func() {
		replies <- managerCall(manager, http.MethodPost, first.private+refreshPath, writer, cmd1, body)
	}()
	go func() {
		replies <- managerCall(manager, http.MethodPost, second.private+refreshPath, other, cmd2, body)
	}()
	for range 2 {
		reply := <-replies
		reply.requireStatus(t, http.StatusAccepted)
		op := managerJSON[accountOperation](t, reply.body)
		if op.State != "accepted" || op.Target.ID != account || (op.ID != cmd1.id && op.ID != cmd2.id) ||
			reply.location != path(managerTestCommand{id: op.ID}) {
			t.Fatalf("bad refresh admission: %+v, Location %q", op, reply.location)
		}
	}
	replay := managerRequest(t, manager, http.MethodPost, second.private+refreshPath, writer, cmd1, body, http.StatusAccepted)
	again := managerRequest(t, manager, http.MethodPost, first.private+refreshPath, writer, cmd1, body, http.StatusAccepted)
	if !bytes.Equal(replay.body, again.body) || replay.location != again.location {
		t.Fatalf("refresh replay changed receipt bytes: %s / %s", replay.body, again.body)
	}
	var jobID uuid.UUID
	mustManager(t, owner.QueryRow(ctx,
		`SELECT DISTINCT job_id FROM platform_manager.account_refresh_work WHERE command_id IN ($1,$2)`, cmd1.id, cmd2.id).Scan(&jobID))
	var distinctJobs int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(DISTINCT job_id) FROM platform_manager.account_refresh_work WHERE command_id IN ($1,$2)`,
		cmd1.id, cmd2.id).Scan(&distinctJobs))
	if distinctJobs != 1 {
		t.Fatalf("two replicas scheduled %d jobs for the same component", distinctJobs)
	}

	receipt := managerRequest(t, manager, http.MethodGet, second.private+cmd1.receiptPath(), writer, managerTestCommand{}, "", http.StatusOK)
	value := managerJSON[managerReceipt](t, receipt.body)
	if value.ID != cmd1.id || value.Kind != accountRefreshKind || value.State != "accepted" || value.OperationID == nil || *value.OperationID != cmd1.id {
		t.Fatalf("bad command receipt: %+v", value)
	}
	managerRequest(t, manager, http.MethodGet, first.private+path(cmd1), other, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, first.private+path(cmd1), reader, managerTestCommand{}, "", http.StatusForbidden)
	conflict := cmd1
	conflict.target, conflict.version = episode, episodeVersion
	managerRequest(t, manager, http.MethodPut, second.private+conflict.reviewPath(), writer, conflict, `{"reviewed":true}`,
		http.StatusConflict).requireCode(t, "idempotency_conflict")

	worker := startManagerProcess(t, append(env, "PLATFORM_WORKERS=2")...)
	old := nextAccountFixtureCall(t, ctx, gateway)
	if old.req.AmoAccountId != 2542 || old.req.CoreInstallationId != pilot.otherInstallation.String() ||
		old.req.IntegrationId != pilot.otherIntegration.String() || old.req.Generation != 1 || old.req.AuthorizationSequence != 1 {
		t.Fatalf("unexpected gateway read scope: %v", old.req)
	}
	recoverAccountJob(t, ctx, pilot, queue.NewStore(jobs), jobID, false)
	fresh := nextAccountFixtureCall(t, ctx, gateway)
	fresh.answer <- accountFixtureAnswer{users: []*platformpb.ObservedCRMUser{{Id: 10, Name: "Current", IsActive: true, IsAdmin: true}}}
	waitAccountOperation(t, ctx, manager, second.private+path(cmd1), writer, "succeeded")
	waitAccountOperation(t, ctx, manager, first.private+path(cmd2), other, "succeeded")
	old.answer <- accountFixtureAnswer{users: []*platformpb.ObservedCRMUser{{Id: 9, Name: "Expired attempt", IsActive: true}}}
	awaitMetric(ctx, t, &http.Client{Timeout: time.Second}, worker.management, `platform_queue_outcomes_total{outcome="lease_lost"} 1`)
	result := managerRequest(t, manager, http.MethodGet, first.private+usersPath+"?active=true&admin=true", writer, managerTestCommand{}, "", http.StatusOK)
	if !bytes.Contains(result.body, []byte(`"name":"Current"`)) || bytes.Contains(result.body, []byte("Expired attempt")) {
		t.Fatalf("expired lease replaced confirmed roster: %s", result.body)
	}
	replayAfter := managerRequest(t, manager, http.MethodPost, second.private+refreshPath, writer, cmd1, body, http.StatusAccepted)
	if !bytes.Equal(replayAfter.body, replay.body) {
		t.Fatalf("terminal result changed the original 202: %s", replayAfter.body)
	}

	worker.stop(t)
	lastCmd := newManagerTestCommand(account, 0)
	managerRequest(t, manager, http.MethodPost, first.private+refreshPath, writer, lastCmd, body, http.StatusAccepted)
	var finalJob uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT job_id FROM platform_manager.account_refresh_work WHERE command_id=$1`, lastCmd.id).Scan(&finalJob))
	_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET max_attempts=1 WHERE id=$1`, finalJob)
	mustManager(t, err)
	worker = startManagerProcess(t, append(env, "PLATFORM_WORKERS=2")...)
	last := nextAccountFixtureCall(t, ctx, gateway)
	recoverAccountJob(t, ctx, pilot, queue.NewStore(jobs), finalJob, true)
	failed := waitAccountOperation(t, ctx, manager, second.private+path(lastCmd), writer, "failed")
	if failed.Error == nil || failed.Error.Error.Code != "lease_expired" {
		t.Fatalf("final lease expiry did not terminate operation: %+v", failed)
	}

	nextCmd := newManagerTestCommand(account, 0)
	managerRequest(t, manager, http.MethodPost, first.private+refreshPath, writer, nextCmd, body, http.StatusAccepted)
	throttled := nextAccountFixtureCall(t, ctx, gateway)
	backoff, err := status.New(codes.ResourceExhausted, "test rate limit").WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(3 * time.Second)})
	mustManager(t, err)
	throttled.answer <- accountFixtureAnswer{err: backoff.Err()}
	waiting := waitAccountOperation(t, ctx, manager, second.private+path(nextCmd), writer, "waiting_external")
	if waiting.RetryAfter < 1 {
		t.Fatalf("missing durable Retry-After: %+v", waiting)
	}
	result = managerRequest(t, manager, http.MethodGet, first.private+usersPath, writer, managerTestCommand{}, "", http.StatusOK)
	if !bytes.Contains(result.body, []byte(`"state":"stale"`)) || !bytes.Contains(result.body, []byte(`"name":"Current"`)) {
		t.Fatalf("throttle replaced the last confirmed roster: %s", result.body)
	}
	last.answer <- accountFixtureAnswer{users: []*platformpb.ObservedCRMUser{{Id: 11, Name: "Dead attempt", IsActive: true}}}
	awaitMetric(ctx, t, &http.Client{Timeout: time.Second}, worker.management, `platform_queue_outcomes_total{outcome="lease_lost"} 1`)
	confirmedEmpty := nextAccountFixtureCall(t, ctx, gateway)
	confirmedEmpty.answer <- accountFixtureAnswer{}
	waitAccountOperation(t, ctx, manager, second.private+path(nextCmd), writer, "succeeded")
	result = managerRequest(t, manager, http.MethodGet, first.private+usersPath, writer, managerTestCommand{}, "", http.StatusOK)
	if !bytes.Contains(result.body, []byte(`"value":[]`)) || !bytes.Contains(result.body, []byte(`"state":"fresh"`)) {
		t.Fatalf("successful explicit empty roster was not retained: %s", result.body)
	}

	worker.stop(t)
	exhausted := newManagerTestCommand(account, 0)
	managerRequest(t, manager, http.MethodPost, first.private+refreshPath, writer, exhausted, body, http.StatusAccepted)
	_, err = owner.Exec(ctx, `UPDATE platform_jobs.jobs SET max_attempts=1
		WHERE id=(SELECT job_id FROM platform_manager.account_refresh_work WHERE command_id=$1)`, exhausted.id)
	mustManager(t, err)
	startManagerProcess(t, append(env, "PLATFORM_WORKERS=2")...)
	finished := nextAccountFixtureCall(t, ctx, gateway)
	finished.answer <- accountFixtureAnswer{err: backoff.Err()}
	exhaustedOperation := waitAccountOperation(t, ctx, manager, second.private+path(exhausted), writer, "failed")
	if exhaustedOperation.Error == nil || exhaustedOperation.Error.Error.Code != "source_rate_limited" {
		t.Fatalf("last attempt did not preserve the rate-limit failure: %+v", exhaustedOperation)
	}
	var cooldownUntil, attemptedAt time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT j.available_at,j.updated_at FROM platform_jobs.jobs j
		JOIN platform_manager.account_refresh_work w ON w.job_id=j.id WHERE w.command_id=$1`, exhausted.id).
		Scan(&cooldownUntil, &attemptedAt))
	if cooldownUntil.Sub(attemptedAt) < 2900*time.Millisecond {
		t.Fatalf("last attempt dropped the supplier cooldown: available %s, finished %s", cooldownUntil, attemptedAt)
	}
	afterCooldown := newManagerTestCommand(account, 0)
	managerRequest(t, manager, http.MethodPost, second.private+refreshPath, other, afterCooldown, body, http.StatusAccepted)
	var nextAvailable time.Time
	var attempts int
	mustManager(t, owner.QueryRow(ctx, `SELECT j.available_at,j.attempt FROM platform_jobs.jobs j
		JOIN platform_manager.account_refresh_work w ON w.job_id=j.id WHERE w.command_id=$1`, afterCooldown.id).
		Scan(&nextAvailable, &attempts))
	if !nextAvailable.Equal(cooldownUntil) || attempts != 0 {
		t.Fatalf("new command bypassed the cooldown: available %s, want %s, attempts %d", nextAvailable, cooldownUntil, attempts)
	}
	waiting = waitAccountOperation(t, ctx, manager, first.private+path(afterCooldown), other, "waiting_external")
	if waiting.RetryAfter < 1 {
		t.Fatalf("new command did not report its inherited cooldown: %+v", waiting)
	}
	afterBackoff := nextAccountFixtureCall(t, ctx, gateway)
	var claimedAt time.Time
	mustManager(t, owner.QueryRow(ctx, `SELECT j.updated_at FROM platform_jobs.jobs j
		JOIN platform_manager.account_refresh_work w ON w.job_id=j.id
		WHERE w.command_id=$1 AND j.status='running'`, afterCooldown.id).Scan(&claimedAt))
	if claimedAt.Before(cooldownUntil) {
		t.Fatalf("worker called the Gateway from a lease claimed at %s before the cooldown ended at %s", claimedAt, cooldownUntil)
	}
	afterBackoff.answer <- accountFixtureAnswer{}
	waitAccountOperation(t, ctx, manager, second.private+path(afterCooldown), other, "succeeded")

	listURL := managerPath + "accounts?limit=1&sort=amo_account_id"
	page := managerJSON[accountPage](t, managerRequest(t, manager, http.MethodGet, first.private+listURL,
		writer, managerTestCommand{}, "", http.StatusOK).body)
	if page.Summary.Total != 2 || page.Next == nil || len(page.Items) != 1 ||
		!bytes.Contains(page.Items[0], []byte(`"amo_account_id":"2542"`)) {
		t.Fatalf("first account page did not include its cross-replica cursor: %+v", page)
	}
	secondPageURL := listURL + "&cursor=" + url.QueryEscape(*page.Next)
	secondPage := managerJSON[accountPage](t, managerRequest(t, manager, http.MethodGet, second.private+secondPageURL,
		writer, managerTestCommand{}, "", http.StatusOK).body)
	if !reflect.DeepEqual(secondPage.Summary, page.Summary) || !secondPage.SnapshotAt.Equal(page.SnapshotAt) || secondPage.Next != nil ||
		len(secondPage.Items) != 1 || !bytes.Contains(secondPage.Items[0], []byte(`"amo_account_id":"9007199254740993"`)) {
		t.Fatalf("account continuation changed its snapshot: %+v", secondPage)
	}
	detail := managerRequest(t, manager, http.MethodGet, first.private+managerPath+"accounts/"+account.String(),
		writer, managerTestCommand{}, "", http.StatusOK)
	if detail.etag != `"1"` || !bytes.Contains(detail.body, []byte(`"state":"unsupported"`)) {
		t.Fatalf("account detail or provenance missing: %s", detail.body)
	}
	license, err := time.Parse(time.RFC3339, "2030-03-15T00:00:00+03:00")
	mustManager(t, err)
	tx, err := jobs.Begin(ctx)
	mustManager(t, err)
	defer tx.Rollback(ctx)
	outcome, err := publishAmoStart(ctx, tx, account, 2542, amostart.License{Until: license, Tariff: "Расширенный"})
	mustManager(t, err)
	if outcome.FailureCode != "" {
		t.Fatalf("amoSTART publication failed: %+v", outcome)
	}
	mustManager(t, tx.Commit(ctx))
	accountURL := first.private + managerPath + "accounts/" + account.String()
	assertLicense := func(wantState string) {
		t.Helper()
		detail := managerRequest(t, manager, http.MethodGet, accountURL, writer, managerTestCommand{}, "", http.StatusOK)
		var body struct {
			License struct {
				State, Source string
				Value         struct {
					Until time.Time `json:"license_until"`
				}
			}
		}
		mustManager(t, json.Unmarshal(detail.body, &body))
		if body.License.State != wantState || body.License.Source != "amoSTART" || !body.License.Value.Until.Equal(license) {
			t.Fatalf("unexpected license observation: %s", detail.body)
		}
	}
	assertLicense("fresh")
	_, err = owner.Exec(ctx, `INSERT INTO platform_manager.account_refresh_slots(account_id,component,result_code) VALUES($1,'amostart','amostart_unavailable')`, account)
	mustManager(t, err)
	assertLicense("stale")
	managerRequest(t, manager, http.MethodGet, second.private+secondPageURL, reader, managerTestCommand{}, "", http.StatusBadRequest)

	_, err = owner.Exec(ctx, `INSERT INTO platform_manager.account_domains(account_id,domain) VALUES($1,'previous.amocrm.ru')`, account)
	mustManager(t, err)
	matched := managerJSON[accountPage](t, managerRequest(t, manager, http.MethodGet,
		first.private+managerPath+"accounts?q=PREVIOUS&limit=1", writer, managerTestCommand{}, "", http.StatusOK).body)
	if matched.Summary.Total != 1 || len(matched.Items) != 1 || matched.Next != nil ||
		!bytes.Contains(matched.Items[0], []byte(`"domain_aliases":["previous.amocrm.ru"]`)) {
		t.Fatalf("alias search did not return the only matching account: %+v", matched)
	}
	empty := managerJSON[accountPage](t, managerRequest(t, manager, http.MethodGet,
		second.private+managerPath+"accounts?q=unmatched-domain", writer, managerTestCommand{}, "", http.StatusOK).body)
	if empty.Summary.Total != 0 || empty.Items == nil || len(empty.Items) != 0 || empty.Next != nil {
		t.Fatalf("short empty search did not return a present empty items array: %+v", empty)
	}

	_, err = owner.Exec(ctx, `DELETE FROM platform_lifecycle.scope WHERE integration_id=$1 AND amo_account_id=2542`, pilot.otherIntegration)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, first.private+usersPath, writer, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, second.private+path(nextCmd), writer, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, first.private+nextCmd.receiptPath(), writer, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, second.private+secondPageURL, writer, managerTestCommand{}, "", http.StatusBadRequest)
}

func recoverAccountJob(t *testing.T, ctx context.Context, p managerPilot, store queue.Store, id uuid.UUID, final bool) {
	t.Helper()
	_, err := p.owner.Exec(ctx, `UPDATE platform_jobs.jobs SET lease_until=statement_timestamp()-interval '1 second'
		WHERE id=$1 AND status='running'`, id)
	mustManager(t, err)
	_, _, err = store.Recover(ctx)
	mustManager(t, err)
	var state queue.Status
	mustManager(t, p.owner.QueryRow(ctx, `SELECT status FROM platform_jobs.jobs WHERE id=$1`, id).Scan(&state))
	if final && state != queue.Dead || !final && state != queue.Queued && state != queue.Running {
		t.Fatalf("unexpected expired lease recovery: %s", state)
	}
}

func waitAccountOperation(t *testing.T, ctx context.Context, manager *http.Client, url string, actor managerTestActor, state string) accountOperation {
	t.Helper()
	var op accountOperation
	for ctx.Err() == nil {
		result := managerRequest(t, manager, http.MethodGet, url, actor, managerTestCommand{}, "", http.StatusOK)
		op = managerJSON[accountOperation](t, result.body)
		if op.State == state {
			return op
		}
		if op.State == "failed" || op.State == "succeeded" {
			t.Fatalf("operation ended as %s, want %s: %s", op.State, state, result.body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waited for %s, last operation state %+v", state, op)
	return op
}

type accountFixtureGateway struct {
	platformpb.UnimplementedCoreAccountObservationsServer
	calls chan accountFixtureCall
}

type accountFixtureCall struct {
	req    *platformpb.ObserveAccountRequest
	answer chan accountFixtureAnswer
}

type accountFixtureAnswer struct {
	users []*platformpb.ObservedCRMUser
	err   error
}

func (g *accountFixtureGateway) ObserveAccount(ctx context.Context, req *platformpb.ObserveAccountRequest) (*platformpb.AccountObservation, error) {
	if req.Component == platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_LICENSE {
		return &platformpb.AccountObservation{ObservedAt: timestamppb.Now(), Result: &platformpb.AccountObservation_Identity{
			Identity: &platformpb.AccountIdentity{CurrentDomain: "pilot.amocrm.ru", DisplayName: "Pilot account"},
		}}, nil
	}
	call := accountFixtureCall{req: req, answer: make(chan accountFixtureAnswer, 1)}
	select {
	case g.calls <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-call.answer:
		if result.err != nil {
			return nil, result.err
		}
		return &platformpb.AccountObservation{ObservedAt: timestamppb.Now(), Result: &platformpb.AccountObservation_Roster{
			Roster: &platformpb.CRMUserRoster{Users: result.users},
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func startAccountFixtureGateway(t *testing.T, certs string, gateway *accountFixtureGateway) string {
	t.Helper()
	dir := filepath.Join(certs, "core")
	cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "", true)
	mustManager(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	mustManager(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	platformpb.RegisterCoreAccountObservationsServer(server, gateway)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func nextAccountFixtureCall(t *testing.T, ctx context.Context, gateway *accountFixtureGateway) accountFixtureCall {
	t.Helper()
	select {
	case call := <-gateway.calls:
		return call
	case <-ctx.Done():
		t.Fatalf("Core Gateway received no roster request: %v", ctx.Err())
		return accountFixtureCall{}
	}
}
