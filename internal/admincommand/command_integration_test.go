package admincommand

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/adminread"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

const fixtureSecret = "fixture-client-private-value"
const fixtureActor = "employee:11111111-1111-4111-8111-111111111111"

func fixture(t *testing.T) (*Store, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
	t.Helper()
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys, err := cryptox.ParseKeyRing("1:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", 1)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool, keys, 5*time.Second, "https://core.example.invalid", nil, nil)
	redirect := "https://relay.example.invalid/oauth/amocrm/callback"
	integration, err := integrations.NewStore(pool, keys).Apply(t.Context(), integrations.Command{Action: "create", Actor: "fixture", Code: "fixture-admin", ClientID: uuid.NewString(), Secret: []byte(fixtureSecret), RedirectURI: &redirect, Services: []string{"lead-status", "activity"}})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,91030303,'admin-commands.amocrm.test','active')`, id, integration.ID); err != nil {
		t.Fatal(err)
	}
	return store, pool, id, integration.ID
}
func request(kind string, id uuid.UUID, command string) Request {
	return Request{TargetType: kind, TargetID: id.String(), Command: command, Payload: json.RawMessage(`{}`)}
}
func execute(t *testing.T, s *Store, req Request) Receipt {
	t.Helper()
	r, err := s.Execute(t.Context(), fixtureActor, uuid.NewString(), req)
	if err != nil {
		t.Fatal(err)
	}
	assertSafeReceipt(t, r)
	return r
}

func assertSafeReceipt(t *testing.T, receipt Receipt) {
	t.Helper()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				for _, forbidden := range []string{"secret", "token", "ciphertext", "key_hash", "password", "payload"} {
					if strings.Contains(strings.ToLower(key), forbidden) {
						t.Errorf("forbidden receipt field %s", key)
					}
				}
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(value)
}

func TestDurableCommandsIdempotencyAndNativeAudit(t *testing.T) {
	s, pool, id, _ := fixture(t)
	key := uuid.NewString()
	req := request("installation", id, "disable")
	first, err := s.Execute(t.Context(), fixtureActor, key, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "succeeded" || first.ID.String() != key {
		t.Fatalf("receipt=%+v", first)
	}
	second, err := s.Execute(t.Context(), "employee:another", key, req)
	if err != nil || second.ID != first.ID {
		t.Fatalf("cross-actor replay=%+v %v", second, err)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE action='installation.disable' AND actor_type='admin' AND actor_id=$1`, fixtureActor).Scan(&count); err != nil || count != 1 {
		t.Fatalf("atomic audit count=%d err=%v", count, err)
	}
	_, err = s.Execute(t.Context(), fixtureActor, key, request("installation", id, "enable"))
	var api *Error
	if !errors.As(err, &api) || api.Code != "conflict" {
		t.Fatalf("changed request=%v", err)
	}
	if r := execute(t, s, request("installation", id, "revoke")); r.State != "failed" || r.Error.Code != "conflict" {
		t.Fatalf("invalid revoke=%+v", r)
	}
	if r := execute(t, s, request("installation", id, "enable")); r.State != "succeeded" {
		t.Fatalf("enable=%+v", r)
	}
	r := execute(t, s, request("installation", id, "revoke"))
	if r.State != "succeeded" || !bytes.Contains(r.Result, []byte(`https://core.example.invalid/oauth/amocrm/start?integration_code=fixture-admin`)) {
		t.Fatalf("revoke=%+v", r)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE installations SET status='active' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"pilot-enable", "pilot-disable", "reconcile"} {
		if r := execute(t, s, request("installation", id, cmd)); r.State != "succeeded" {
			t.Fatalf("%s=%+v", cmd, r)
		}
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_log WHERE actor_type='admin' AND actor_id=$1 AND action IN('activity.pilot','webhook.reconcile.requested')`, fixtureActor).Scan(&count); err != nil || count != 3 {
		t.Fatalf("native audit=%d %v", count, err)
	}
}

func TestIntegrationSecretsNeverEnterReceiptAndMutationRollsBackWithAudit(t *testing.T) {
	s, pool, _, integrationID := fixture(t)
	for _, cmd := range []string{"disable", "enable"} {
		if r := execute(t, s, request("integration", integrationID, cmd)); r.State != "succeeded" {
			t.Fatalf("%s=%+v", cmd, r)
		}
	}
	for _, entry := range []struct{ command, payload string }{
		{"update", `{"webhook_events":["add_lead"]}`},
		{"set-service", `{"service":"activity","enabled":false}`},
		{"rotate-secret", `{"client_secret":"replacement-private-fixture"}`},
	} {
		req := request("integration", integrationID, entry.command)
		req.Payload = []byte(entry.payload)
		r := execute(t, s, req)
		if r.State != "succeeded" {
			t.Fatalf("%s=%+v", entry.command, r)
		}
	}
	create := Request{TargetType: "integration", TargetID: "new", Command: "create", Payload: json.RawMessage(`{"code":"fixture-created","client_id":"44444444-4444-4444-8444-444444444444","client_secret":"receipt-never-stores-this","redirect_uri":"https://core.example.invalid/oauth/amocrm/callback","services":[]}`)}
	key := uuid.NewString()
	r, err := s.Execute(t.Context(), fixtureActor, key, create)
	if err != nil || r.State != "succeeded" {
		t.Fatalf("create=%+v %v", r, err)
	}
	var text string
	if err := pool.QueryRow(t.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(c))::text,'') FROM admin_commands c`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "receipt-never-stores-this") || strings.Contains(text, "replacement-private-fixture") {
		t.Fatal("plaintext persisted in receipt")
	}
	create.Payload = bytes.ReplaceAll(create.Payload, []byte("receipt-never-stores-this"), []byte("changed-private-value"))
	if _, err := s.Execute(t.Context(), fixtureActor, key, create); err == nil {
		t.Fatal("secret change must conflict")
	}
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION reject_admin_domain_audit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.action='integration.disable' AND NEW.actor_type='admin' THEN RAISE EXCEPTION 'fixture audit failure'; END IF; RETURN NEW; END$$;
		CREATE TRIGGER reject_admin_domain_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION reject_admin_domain_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER reject_admin_domain_audit ON audit_log; DROP FUNCTION reject_admin_domain_audit()`)
	})
	failed := execute(t, s, request("integration", integrationID, "disable"))
	if failed.State != "failed" {
		t.Fatalf("audit failure=%+v", failed)
	}
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM integrations WHERE id=$1`, integrationID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("mutation not rolled back: %s %v", status, err)
	}
}

func TestConcurrentReplayAndPendingTargetConflict(t *testing.T) {
	s, _, id, _ := fixture(t)
	key := uuid.NewString()
	req := request("installation", id, "check")
	const n = 6
	receipts := make(chan Receipt, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Execute(t.Context(), fixtureActor, key, req)
			receipts <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for r := range receipts {
		if r.ID.String() != key || r.State != "pending" || r.JobID == nil {
			t.Fatalf("replay=%+v", r)
		}
	}
	if _, err := s.Execute(t.Context(), fixtureActor, uuid.NewString(), request("installation", id, "disable")); err == nil {
		t.Fatal("pending check must serialize target")
	}
	got, err := s.Get(t.Context(), uuid.MustParse(key))
	if err != nil || got.State != "pending" {
		t.Fatalf("read-only receipt recovery=%+v %v", got, err)
	}
}

func TestWorkerCompletionPartialUninstallAndExpiredLease(t *testing.T) {
	s, pool, id, _ := fixture(t)
	jobStore := jobs.NewStore(pool)
	r := execute(t, s, request("installation", id, "check"))
	claimed, err := jobStore.Claim(t.Context(), "fixture-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%v %v", claimed, err)
	}
	var calls int
	executor := &WorkerExecutor{pool: pool, Check: func(context.Context, uuid.UUID) error { calls++; return nil }}
	raw, err := executor.Handler(t.Context(), claimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.CompleteWithObserver(t.Context(), claimed[0], "fixture-worker", raw, time.Millisecond, CompleteReceipt); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(t.Context(), r.ID)
	if err != nil || got.State != "succeeded" || got.Outcome != "verified_ok" || calls != 1 {
		t.Fatalf("check=%+v calls=%d %v", got, calls, err)
	}
	if err := jobStore.CompleteWithObserver(t.Context(), claimed[0], "fixture-worker", raw, time.Millisecond, CompleteReceipt); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("completion fence=%v", err)
	}
	uninstall := execute(t, s, request("installation", id, "uninstall"))
	if uninstall.State != "pending" {
		t.Fatalf("uninstall=%+v", uninstall)
	}
	var status string
	_ = pool.QueryRow(t.Context(), `SELECT status FROM installations WHERE id=$1`, id).Scan(&status)
	if status != "uninstalled" {
		t.Fatal("uninstall must commit local state before remote I/O")
	}
	claimed, err = jobStore.Claim(t.Context(), "fixture-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	executor.Unregister = func(context.Context, uuid.UUID) error { return errors.New("sensitive upstream details") }
	raw, err = executor.Handler(t.Context(), claimed[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := jobStore.CompleteWithObserver(t.Context(), claimed[0], "fixture-worker", raw, time.Millisecond, CompleteReceipt); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(t.Context(), uninstall.ID)
	if err != nil || got.State != "partial" || got.Error.Code != "webhook_error" || bytes.Contains(got.Result, []byte("sensitive")) {
		t.Fatalf("partial=%+v %v", got, err)
	}
	again := execute(t, s, request("installation", id, "uninstall"))
	if again.State != "pending" {
		t.Fatalf("uninstall retry=%+v", again)
	}
	claimed, err = jobStore.Claim(t.Context(), "crashed-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE jobs SET locked_until=now()-interval '1 second' WHERE id=$1`, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	_, err = jobStore.ClaimWithObserver(t.Context(), "reaper", 1, 100, time.Minute, FailReceipt)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Get(t.Context(), again.ID)
	if err != nil || got.State != "unknown_outcome" {
		t.Fatalf("crash recovery=%+v %v", got, err)
	}
}

func TestRetryPreservesJobPrincipalAndDeliveryOwner(t *testing.T) {
	s, pool, id, integrationID := fixture(t)
	jobID := uuid.New()
	payload := `{"fixture":"unchanged"}`
	if _, err := pool.Exec(t.Context(), `INSERT INTO jobs(id,installation_id,type,status,actor_type,actor_id,payload,attempts,max_attempts)VALUES($1,$2,'widget.ping','dead','widget_user','42',$3,5,5)`, jobID, id, payload); err != nil {
		t.Fatal(err)
	}
	readRetry := func() adminread.Job {
		router := chi.NewRouter()
		adminread.Register(router, adminread.Dependencies{Pool: pool, Token: "fixture-bearer"})
		req := httptest.NewRequest(http.MethodGet, "/admin/v1/jobs/"+jobID.String(), nil)
		req.Header.Set("Authorization", "Bearer fixture-bearer")
		req.Header.Set("X-Admin-Actor", fixtureActor)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("job read=%d %s", rec.Code, rec.Body.String())
		}
		var body struct{ Job adminread.Job }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Job
	}
	if job := readRetry(); !job.RetryAllowed {
		t.Fatalf("eligible job marked %s", job.RetryReason)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE integrations SET status='disabled' WHERE id=$1`, integrationID); err != nil {
		t.Fatal(err)
	}
	if job := readRetry(); job.RetryAllowed || job.RetryReason != "target_inactive" {
		t.Fatalf("inactive retry metadata=%+v", job)
	}
	if r := execute(t, s, request("job", jobID, "retry")); r.State != "failed" {
		t.Fatal("inactive integration retry accepted")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE integrations SET status='active' WHERE id=$1`, integrationID); err != nil {
		t.Fatal(err)
	}
	r := execute(t, s, request("job", jobID, "retry"))
	if r.State != "succeeded" {
		t.Fatalf("retry=%+v", r)
	}
	var actor, body, status string
	var attempts, maxAttempts int
	if err := pool.QueryRow(t.Context(), `SELECT actor_id,payload::text,status,attempts,max_attempts FROM jobs WHERE id=$1`, jobID).Scan(&actor, &body, &status, &attempts, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if actor != "42" || !strings.Contains(body, "unchanged") || status != "retry" || attempts != 5 || maxAttempts != 10 {
		t.Fatalf("retry mutated principal/history: %s %s %d/%d", actor, status, attempts, maxAttempts)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE jobs SET type='lead.set_status',status='failed' WHERE id=$1`, jobID); err != nil {
		t.Fatal(err)
	}
	if r := execute(t, s, request("job", jobID, "retry")); r.State != "failed" {
		t.Fatal("unsafe type accepted")
	}
	deliveryID := uuid.New()
	hash := sha256.Sum256([]byte(deliveryID.String()))
	if _, err := pool.Exec(t.Context(), `INSERT INTO activity_command_receipts(command_id,installation_id,integration_id,actor_id,target,action,key_hash,request_hash)VALUES($1,$2,$3,42,'activity','settings',$4,$4)`, deliveryID, id, integrationID, hash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO activity_command_outbox(command_id,payload,status)VALUES($1,'{}','failed')`, deliveryID); err != nil {
		t.Fatal(err)
	}
	req := request("delivery", deliveryID, "retry")
	req.Payload = []byte(`{"installation_id":"` + uuid.NewString() + `"}`)
	if r := execute(t, s, req); r.State != "failed" || r.Error.Code != "not_found" {
		t.Fatalf("foreign owner=%+v", r)
	}
	req.Payload = []byte(`{"installation_id":"` + id.String() + `"}`)
	if r := execute(t, s, req); r.State != "succeeded" {
		t.Fatalf("delivery retry=%+v", r)
	}
}

func TestActivityConfigureConflictAndSyncPendingThenTerminal(t *testing.T) {
	s, pool, id, _ := fixture(t)
	if err := activitybridge.SetPilot(t.Context(), pool, id, true); err != nil {
		t.Fatal(err)
	}
	conflictBridge := activitybridge.New(pool, &commandPolicy{}, &conflictActivity{}, &syncEvents{state: "succeeded"})
	s.bridge = conflictBridge
	req := request("installation", id, "activity-configure")
	req.Payload = json.RawMessage(`{"initial_days":2,"retention_days":7,"expected_updated_at":0}`)
	_, err := s.Execute(t.Context(), fixtureActor, uuid.NewString(), req)
	var api *Error
	if !errors.As(err, &api) || api.Code != "conflict" || api.Status != http.StatusConflict {
		t.Fatalf("configure conflict=%v", err)
	}

	s.bridge = activitybridge.New(pool, &commandPolicy{}, &acceptingActivityAdapter{}, &syncEvents{state: "running"})
	syncReq := request("installation", id, "activity-sync")
	syncReq.Payload = json.RawMessage(`{"kind":"sync"}`)
	pending, err := s.Execute(t.Context(), fixtureActor, uuid.NewString(), syncReq)
	if err != nil || pending.State != "pending" {
		t.Fatalf("sync pending=%+v %v", pending, err)
	}
	assertSafeReceipt(t, pending)
	var commandID string
	var result map[string]any
	if err := json.Unmarshal(pending.Result, &result); err != nil {
		t.Fatal(err)
	}
	commandID, _ = result["command_id"].(string)
	if commandID == "" {
		t.Fatalf("missing command_id in %s", pending.Result)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE activity_command_outbox SET status='accepted' WHERE command_id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	s.bridge = activitybridge.New(pool, &commandPolicy{}, &acceptingActivityAdapter{}, &syncEvents{state: "succeeded"})
	got, err := s.Get(t.Context(), pending.ID)
	if err != nil || got.State != "succeeded" {
		t.Fatalf("sync terminal=%+v %v", got, err)
	}
}

// prepareUnreadActivitySync leaves the admin receipt pending, then makes the
// outbox accepted without reading the receipt again.
func prepareUnreadActivitySync(t *testing.T, s *Store, pool *pgxpool.Pool, id uuid.UUID, events *syncEvents) (Request, Receipt) {
	t.Helper()
	if err := activitybridge.SetPilot(t.Context(), pool, id, true); err != nil {
		t.Fatal(err)
	}
	s.bridge = activitybridge.New(pool, &commandPolicy{}, &acceptingActivityAdapter{}, events)
	req := request("installation", id, "activity-sync")
	req.Payload = json.RawMessage(`{"kind":"sync"}`)
	pending := execute(t, s, req)
	if pending.State != "pending" {
		t.Fatalf("sync=%+v", pending)
	}
	var result struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(pending.Result, &result); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE activity_command_outbox SET status='accepted' WHERE command_id=$1`, result.CommandID); err != nil {
		t.Fatal(err)
	}
	return req, pending
}

func TestExecuteReconcilesUnreadActivitySync(t *testing.T) {
	for _, tc := range []struct {
		name, storedState, remoteState, wantState string
		remoteError                               error
		wantConflict                              bool
	}{
		{name: "completed_pending", storedState: "pending", remoteState: "succeeded", wantState: "succeeded"},
		{name: "completed_running", storedState: "running", remoteState: "succeeded", wantState: "succeeded"},
		{name: "failed", storedState: "pending", remoteState: "failed", wantState: "failed"},
		{name: "running", storedState: "pending", remoteState: "running", wantState: "running", wantConflict: true},
		{name: "unavailable", storedState: "pending", remoteError: serviceapi.Fail(serviceapi.Unavailable, "fixture unavailable"), wantState: "running", wantConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool, id, _ := fixture(t)
			syncReq, pending := prepareUnreadActivitySync(t, s, pool, id, &syncEvents{state: tc.remoteState, err: tc.remoteError})
			if _, err := pool.Exec(t.Context(), `UPDATE admin_commands SET state=$2 WHERE id=$1`, pending.ID, tc.storedState); err != nil {
				t.Fatal(err)
			}
			checkReq := request("installation", id, "check")
			key := uuid.NewString()
			check, err := s.Execute(t.Context(), fixtureActor, key, checkReq)
			if tc.wantConflict {
				var api *Error
				if !errors.As(err, &api) || api.Code != "conflict" || api.Status != http.StatusConflict {
					t.Fatalf("active sync must block check: %+v %v", check, err)
				}
			} else {
				if err != nil || check.State != "pending" || check.JobID == nil {
					t.Fatalf("completed sync must admit check without GET: %+v %v", check, err)
				}
				replay, err := s.Execute(t.Context(), fixtureActor, key, checkReq)
				if err != nil || replay.ID != check.ID || replay.JobID == nil || *replay.JobID != *check.JobID {
					t.Fatalf("check replay=%+v %v", replay, err)
				}
			}
			// Read SQL directly: Get would repair the state and hide this regression.
			stored, _, err := loadReceipt(t.Context(), pool, `c.id=$1`, pending.ID)
			if err != nil || stored.State != tc.wantState || (stored.FinishedAt != nil) == tc.wantConflict {
				t.Fatalf("persisted sync=%+v %v", stored, err)
			}
			replay, err := s.Execute(t.Context(), fixtureActor, pending.ID.String(), syncReq)
			if err != nil || replay.ID != pending.ID {
				t.Fatalf("sync replay=%+v %v", replay, err)
			}
			changed := request("installation", id, "disable")
			if _, err := s.Execute(t.Context(), fixtureActor, pending.ID.String(), changed); err == nil {
				t.Fatal("changed request with original sync key must conflict")
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM activity_command_receipts WHERE installation_id=$1`, id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("sync executed again: count=%d %v", count, err)
			}
		})
	}
}

func TestConcurrentExecuteAfterUnreadActivitySync(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_key_%t", sameKey), func(t *testing.T) {
			s, pool, id, _ := fixture(t)
			prepareUnreadActivitySync(t, s, pool, id, &syncEvents{state: "succeeded"})
			const n = 6
			key := uuid.NewString()
			start := make(chan struct{})
			type result struct {
				receipt Receipt
				err     error
			}
			results := make(chan result, n)
			for range n {
				go func() {
					<-start
					commandKey := key
					if !sameKey {
						commandKey = uuid.NewString()
					}
					r, err := s.Execute(t.Context(), fixtureActor, commandKey, request("installation", id, "check"))
					results <- result{r, err}
				}()
			}
			close(start)
			accepted := 0
			for range n {
				result := <-results
				if result.err == nil {
					accepted++
					if result.receipt.State != "pending" || result.receipt.JobID == nil || (sameKey && result.receipt.ID.String() != key) {
						t.Errorf("check=%+v", result.receipt)
					}
				} else {
					var api *Error
					if sameKey || !errors.As(result.err, &api) || api.Code != "conflict" {
						t.Errorf("execute=%v", result.err)
					}
				}
			}
			want := 1
			if sameKey {
				want = n
			}
			if accepted != want {
				t.Fatalf("accepted=%d want=%d", accepted, want)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM jobs WHERE installation_id=$1 AND type=$2`, id, CheckJobType).Scan(&count); err != nil || count != 1 {
				t.Fatalf("check jobs=%d %v", count, err)
			}
		})
	}
}

func TestExecuteReconcilesActivitySyncWithSingleConnection(t *testing.T) {
	s, pool, id, _ := fixture(t)
	events := &syncEvents{state: "succeeded"}
	prepareUnreadActivitySync(t, s, pool, id, events)
	config := pool.Config()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	s.pool = single
	s.bridge = activitybridge.New(single, &commandPolicy{}, &acceptingActivityAdapter{}, events)
	// A transaction or unclosed rows during reconciliation would exhaust this
	// pool before bridge status lookup and prevent the check from being admitted.
	check := execute(t, s, request("installation", id, "check"))
	if check.State != "pending" || check.JobID == nil {
		t.Fatalf("check=%+v", check)
	}
}

func TestCommandHTTPAfterUnreadActivitySync(t *testing.T) {
	s, pool, id, _ := fixture(t)
	prepareUnreadActivitySync(t, s, pool, id, &syncEvents{state: "succeeded"})
	router := chi.NewRouter()
	adminread.Register(router, adminread.Dependencies{Pool: pool, Token: "fixture-admin-bearer"})
	Register(router, s)
	body, err := json.Marshal(request("installation", id, "check"))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-admin-bearer")
	req.Header.Set("X-Admin-Actor", fixtureActor)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != http.StatusAccepted {
		t.Fatalf("check=%d %s", out.Code, out.Body.String())
	}
}

type commandPolicy struct{}

func (commandPolicy) Issue(context.Context, serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return serviceapi.Auth{Token: "fixture"}, nil
}
func (commandPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return serviceapi.Principal{}, nil
}

type conflictActivity struct{ serviceapi.Activity }

func (conflictActivity) Settings(context.Context, serviceapi.Auth) (serviceapi.Settings, error) {
	return serviceapi.DefaultSettings(), nil
}
func (conflictActivity) Configure(context.Context, serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "settings were updated")
}

type acceptingActivityAdapter struct{ serviceapi.Activity }

func (acceptingActivityAdapter) Settings(context.Context, serviceapi.Auth) (serviceapi.Settings, error) {
	return serviceapi.DefaultSettings(), nil
}
func (acceptingActivityAdapter) Configure(_ context.Context, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	return serviceapi.Operation{ID: c.CommandID, CommandID: c.CommandID, State: serviceapi.OperationSucceeded}, nil
}

type syncEvents struct {
	serviceapi.CRMEvents
	state string
	err   error
}

func (s *syncEvents) Status(context.Context, serviceapi.Auth) (serviceapi.SyncStatus, error) {
	return serviceapi.SyncStatus{State: "idle"}, nil
}
func (s *syncEvents) Operation(_ context.Context, r serviceapi.OperationRequest) (serviceapi.Operation, error) {
	return serviceapi.Operation{ID: r.OperationID, CommandID: r.OperationID, State: s.state}, s.err
}

func TestWorkerPersistsCheckAndReplayDoesNotCallUpstreamAgain(t *testing.T) {
	s, pool, id, _ := fixture(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v4/account" {
			t.Errorf("unexpected check request %s %s", r.Method, r.URL.Path)
		}
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	executor := &WorkerExecutor{pool: pool, Check: func(ctx context.Context, _ uuid.UUID) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/api/v4/account", nil)
		if err != nil {
			return err
		}
		resp, err := upstream.Client().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return nil
	}}
	worker := jobs.NewWorker(jobs.NewStore(pool), slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.WorkerConfig{
		ID: "fixture-command-worker", PollInterval: 10 * time.Millisecond, LeaseDuration: 3 * time.Second, JobTimeout: 5 * time.Second,
		BatchSize: 1, ReapBatchSize: 100, Concurrency: 1, IntegrationConcurrency: 1, DrainTimeout: 5 * time.Second, ClaimTimeout: 2 * time.Second,
	}, map[string]jobs.Handler{CheckJobType: executor.Handler}, map[string]jobs.FailureObserver{CheckJobType: FailReceipt})
	worker.SetCompletionObservers(map[string]jobs.CompletionObserver{CheckJobType: CompleteReceipt})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not drain")
		}
	}()
	key := uuid.NewString()
	req := request("installation", id, "check")
	r, err := s.Execute(t.Context(), fixtureActor, key, req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err = s.Get(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == "succeeded" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.State != "succeeded" || r.Outcome != "verified_ok" {
		t.Fatalf("worker receipt=%+v", r)
	}
	replay, err := s.Execute(t.Context(), fixtureActor, key, req)
	if err != nil || replay.ID != r.ID || calls.Load() != 1 {
		t.Fatalf("replay calls=%d error=%v", calls.Load(), err)
	}
}

func TestCommandHTTPAuthenticationAndReceiptRecovery(t *testing.T) {
	s, pool, id, _ := fixture(t)
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	adminread.Register(router, adminread.Dependencies{Pool: pool, Token: "fixture-admin-bearer"})
	Register(router, s)
	path := "/admin/v1/commands"
	body, _ := json.Marshal(request("installation", id, "check"))
	key := uuid.NewString()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauth=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture-admin-bearer")
	req.Header.Set("X-Admin-Actor", fixtureActor)
	req.Header.Set("Idempotency-Key", key)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("post=%d %s", rec.Code, rec.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, path+"/"+key, nil)
	get.Header = req.Header.Clone()
	out := httptest.NewRecorder()
	router.ServeHTTP(out, get)
	if out.Code != 200 {
		t.Fatalf("get=%d %s", out.Code, out.Body.String())
	}
	var receipt Receipt
	if err := json.NewDecoder(io.NopCloser(out.Body)).Decode(&receipt); err != nil || receipt.ID.String() != key {
		t.Fatalf("recovery=%+v %v", receipt, err)
	}
}
