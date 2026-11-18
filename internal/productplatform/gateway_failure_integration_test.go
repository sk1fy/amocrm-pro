package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPlatformGatewayFailures(t *testing.T) {
	for _, key := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(key) == "" {
			t.Skip("run scripts/platform-recovery-test.sh for Gateway process checks")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	_, err = owner.Exec(ctx, `DELETE FROM platform_jobs.jobs WHERE kind='accounts.refresh'`)
	mustManager(t, err)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	gateway := &accountFixtureGateway{calls: make(chan accountFixtureCall, 8)}
	address := startAccountFixtureGateway(t, certs, gateway)
	env := []string{"PLATFORM_ACCOUNT_GATEWAY_ADDRESS=" + address, "PLATFORM_ACCOUNT_GATEWAY_IDENTITY_DIR=" + filepath.Join(certs, "platform"), "PLATFORM_ACCOUNT_GATEWAY_SERVER_NAME=localhost"}
	keyConfig, key := recoveryAccessKeys(t)
	env = append(env, "PLATFORM_ACCESS_PROOF_PUBLIC_KEYS="+keyConfig)
	api := startManagerProcess(t, env...)
	client := tlsClient(t, certs, "manager-bff")
	pilot.deliver(t, tlsClient(t, certs, "core"), api.private)
	var account uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=2542`).Scan(&account))
	actor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	refresh := api.private + managerPath + "accounts/" + account.String() + "/refresh"
	users := api.private + managerPath + "accounts/" + account.String() + "/crm-users"
	const input = `{"components":["users"],"reason":"Synthetic Gateway recovery"}`
	operation := func(cmd managerTestCommand) string {
		return api.private + managerPath + "operations/" + cmd.id.String()
	}
	seed := newManagerTestCommand(account, 0)
	managerRequest(t, client, http.MethodPost, refresh, actor, seed, input, http.StatusAccepted)
	worker := startManagerProcess(t, append(env, "PLATFORM_WORKERS=1")...)
	call := nextAccountFixtureCall(t, ctx, gateway)
	call.answer <- accountFixtureAnswer{users: []*platformpb.ObservedCRMUser{{Id: 10, Name: "Last confirmed user", IsActive: true}}}
	waitAccountOperation(t, ctx, client, operation(seed), actor, "succeeded")
	good := managerRequest(t, client, http.MethodGet, users, actor, managerTestCommand{}, "", http.StatusOK)
	if !bytes.Contains(good.body, []byte(`"state":"fresh"`)) || !bytes.Contains(good.body, []byte(`"name":"Last confirmed user"`)) {
		t.Fatalf("missing initial confirmed observation: %s", good.body)
	}
	worker.stop(t)
	for _, failure := range []struct {
		code codes.Code
		want string
	}{{codes.Unavailable, "source_unavailable"}, {codes.DeadlineExceeded, "source_timeout"}} {
		t.Run(failure.want, func(t *testing.T) {
			cmd := newManagerTestCommand(account, 0)
			admitted := managerRequest(t, client, http.MethodPost, refresh, actor, cmd, input, http.StatusAccepted)
			worker = startManagerProcess(t, append(env, "PLATFORM_WORKERS=1")...)
			failedCall := nextAccountFixtureCall(t, ctx, gateway)
			failedCall.answer <- accountFixtureAnswer{err: status.Error(failure.code, "synthetic unavailable dependency")}
			waiting := waitAccountOperation(t, ctx, client, operation(cmd), actor, "waiting_external")
			if waiting.RetryAfter < 1 || waiting.Error != nil {
				t.Fatalf("transient Gateway failure lost retry semantics: %+v", waiting)
			}
			worker.stop(t)
			var lastFailure string
			var attempt int
			mustManager(t, owner.QueryRow(ctx, `SELECT j.failure_code,j.attempt FROM platform_jobs.jobs j JOIN platform_manager.account_refresh_work w ON w.job_id=j.id WHERE w.command_id=$1`, cmd.id).Scan(&lastFailure, &attempt))
			if lastFailure != failure.want || attempt != 1 {
				t.Fatalf("durable failure=%s attempt=%d", lastFailure, attempt)
			}
			stale := managerRequest(t, client, http.MethodGet, users, actor, managerTestCommand{}, "", http.StatusOK)
			if !bytes.Contains(stale.body, []byte(`"state":"stale"`)) || !bytes.Contains(stale.body, []byte(`"name":"Last confirmed user"`)) || !bytes.Contains(stale.body, []byte(`"error_code":"`+failure.want+`"`)) {
				t.Fatalf("dependency failure replaced last good roster: %s", stale.body)
			}
			// Compare the successful observation itself, retaining provenance and time as well as value.
			goodPage := managerJSON[map[string]any](t, good.body)
			stalePage := managerJSON[map[string]any](t, stale.body)
			requireRetainedObservation(t, goodPage, stalePage)
			access := recoveryPublicAccess(t, ctx, api.public, key, platformaccess.Scope{Account: "2542", Product: "other-review", Integration: pilot.otherIntegration, Installation: pilot.otherInstallation, Generation: 1, AuthorizationSequence: 1})
			if access.Active || access.Source != "none" {
				t.Fatalf("Gateway failure changed independent access facts: %+v", access)
			}
			expectStatus(t, &http.Client{Timeout: time.Second}, api.public+"/ready", http.StatusOK)
			expectStatus(t, &http.Client{Timeout: time.Second}, api.management+"/ready", http.StatusOK)
			replay := managerRequest(t, client, http.MethodPost, refresh, actor, cmd, input, http.StatusAccepted)
			if !bytes.Equal(admitted.body, replay.body) || admitted.location != replay.location {
				t.Fatal("dependency failure changed original asynchronous admission receipt")
			}
			worker = startManagerProcess(t, append(env, "PLATFORM_WORKERS=1")...)
			retried := nextAccountFixtureCall(t, ctx, gateway)
			retried.answer <- accountFixtureAnswer{users: []*platformpb.ObservedCRMUser{{Id: 10, Name: "Last confirmed user", IsActive: true}}}
			waitAccountOperation(t, ctx, client, operation(cmd), actor, "succeeded")
			worker.stop(t)
			good = managerRequest(t, client, http.MethodGet, users, actor, managerTestCommand{}, "", http.StatusOK)
		})
	}
}

func requireRetainedObservation(t *testing.T, good, stale map[string]any) {
	t.Helper()
	for _, key := range []string{"observed_at", "fresh_until", "source", "evidence_id", "value"} {
		before, exists := good[key]
		if !exists {
			t.Fatalf("successful roster missing observation %s", key)
		}
		old, err := json.Marshal(before)
		mustManager(t, err)
		current, err := json.Marshal(stale[key])
		mustManager(t, err)
		if !bytes.Equal(old, current) {
			t.Fatalf("Gateway failure changed observation %s", key)
		}
	}
}
