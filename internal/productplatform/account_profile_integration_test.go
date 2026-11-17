package productplatform

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformAccountProfileAcrossReplicas(t *testing.T) {
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
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	first, second := startManagerProcess(t), startManagerProcess(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	manager := tlsClient(t, certs, "manager-bff")
	manager.Timeout = 5 * time.Second
	core := tlsClient(t, certs, "core")
	core.Timeout = 8 * time.Second
	pilot.deliver(t, core, first.private)

	var account uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=2542`).Scan(&account))
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_manager.account_identities(account_id,current_domain,display_name,observed_at)
		VALUES($1,'profile.amocrm.ru','Observed account',statement_timestamp())`, account)
	mustManager(t, err)

	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	revoked := managerTestActor{subject: writer.subject, version: 2, grants: []string{"clients.read"}}
	foreign := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	accountPath := managerPath + "accounts/" + account.String()
	profilePath := accountPath + "/profile"
	start := managerRequest(t, manager, http.MethodGet, first.private+accountPath, writer, managerTestCommand{}, "", http.StatusOK)
	if start.etag != `"1"` || !bytes.Contains(start.body, []byte(`"display_name":"Observed account"`)) {
		t.Fatalf("initial profile did not reflect the observed account: %s", start.body)
	}

	cmd := newManagerTestCommand(account, 1)
	const patch = `{"display_name":"Managed account","classification":"Без партнёра","note":"private note","reason":"private reason"}`
	applied := managerRequest(t, manager, http.MethodPatch, first.private+profilePath, writer, cmd, patch, http.StatusOK)
	if applied.etag != `"2"` || !bytes.Contains(applied.body, []byte(`"capabilities":[]`)) ||
		!bytes.Contains(applied.body, []byte(`"display_name":"Managed account"`)) {
		t.Fatalf("mutation did not return the versioned profile: %s", applied.body)
	}
	first.stop(t)
	first = startManagerProcess(t)
	persisted := managerRequest(t, manager, http.MethodGet, first.private+accountPath, writer, managerTestCommand{}, "", http.StatusOK)
	if persisted.etag != applied.etag || !bytes.Contains(persisted.body, []byte(`"note":"private note"`)) ||
		!bytes.Contains(persisted.body, []byte(`"action":"account.profile"`)) {
		t.Fatalf("profile did not persist after restart: %s", persisted.body)
	}
	lookup := managerRequest(t, manager, http.MethodGet, second.private+managerPath+"accounts?q=managed&classification="+
		url.QueryEscape("Без партнёра"), writer, managerTestCommand{}, "", http.StatusOK)
	page := managerJSON[accountPage](t, lookup.body)
	if page.Summary.Total != 1 || len(page.Items) != 1 || !bytes.Contains(page.Items[0], []byte(`"version":2`)) {
		t.Fatalf("manager list did not use the saved profile: %s", lookup.body)
	}
	replayed := managerRequest(t, manager, http.MethodPatch, first.private+profilePath, writer, cmd,
		`{"reason":"private reason","display_name":" Managed account ","note":"private note","classification":"Без партнёра"}`, http.StatusOK)
	if !bytes.Equal(replayed.body, applied.body) || replayed.etag != applied.etag {
		t.Fatalf("profile returned a different recorded response: %s / %s", applied.body, replayed.body)
	}
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, cmd, `{"reason":"other"}`,
		http.StatusConflict).requireCode(t, "idempotency_conflict")

	stale := newManagerTestCommand(account, 1)
	conflict := managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, stale,
		`{"note":null,"reason":"stale"}`, http.StatusConflict)
	if current := conflict.requireCode(t, "version_conflict").Version; current != 2 {
		t.Fatalf("stale profile mutation returned version %d", current)
	}
	unsupported := newManagerTestCommand(account, 2)
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, unsupported,
		`{"classification":"Наш клиент","reason":"unsupported source"}`, http.StatusUnprocessableEntity).
		requireCode(t, "classification_unavailable")
	unsupportedReceipt := managerRequest(t, manager, http.MethodGet, first.private+unsupported.receiptPath(),
		writer, managerTestCommand{}, "", http.StatusOK)
	if receipt := managerJSON[managerReceipt](t, unsupportedReceipt.body); receipt.State != "rejected" {
		t.Fatalf("ineligible classification did not register the rejection: %s", unsupportedReceipt.body)
	}

	clear := newManagerTestCommand(account, 2)
	cleared := managerRequest(t, manager, http.MethodPatch, first.private+profilePath, writer, clear,
		`{"display_name":null,"note":null,"reason":"clear optional fields"}`, http.StatusOK)
	if cleared.etag != `"3"` || !bytes.Contains(cleared.body, []byte(`"display_name":"Observed account"`)) ||
		!bytes.Contains(cleared.body, []byte(`"note":null`)) || !bytes.Contains(cleared.body, []byte(`"classification":"Без партнёра"`)) {
		t.Fatalf("explicit null or omitted fields were not preserved: %s", cleared.body)
	}
	unchanged := managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, newManagerTestCommand(account, 3),
		`{"note":null,"reason":"already empty"}`, http.StatusOK)
	if unchanged.etag != cleared.etag {
		t.Fatalf("unchanged profile advanced its version: %s", unchanged.etag)
	}
	rejected := managerRequest(t, manager, http.MethodPatch, first.private+profilePath, writer, stale,
		`{"note":null,"reason":"stale"}`, http.StatusConflict)
	if !bytes.Equal(rejected.body, conflict.body) {
		t.Fatalf("stale command replay returned a different version conflict: %s", rejected.body)
	}

	var events, matchingOutbox int
	mustManager(t, owner.QueryRow(ctx, `
		SELECT count(*),count(o.event_id) FILTER (WHERE o.payload->>'id'=e.id::text
			AND o.payload->>'kind'='account_profile_updated' AND o.payload->>'summary'='Обновлён профиль клиента'
			AND NOT(o.payload ?| ARRAY['reason','details','actor_id','note']))
		FROM platform_manager.account_change_events e LEFT JOIN platform_manager.account_event_outbox o ON o.event_id=e.id
		WHERE e.account_id=$1`, account).Scan(&events, &matchingOutbox))
	if events != 2 || matchingOutbox != 2 {
		t.Fatalf("profile changes have %d events and %d safe outbox entries", events, matchingOutbox)
	}
	var auditValues bool
	mustManager(t, owner.QueryRow(ctx, `
		SELECT actor_id=$2 AND reason='private reason' AND details#>>'{after,note}'='private note'
		FROM platform_manager.account_change_events WHERE command_id=$1`, cmd.id, writer.subject).Scan(&auditValues))
	if !auditValues {
		t.Fatal("profile audit lost the actor, reason, or changed values")
	}
	receiptResponse := managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(),
		writer, managerTestCommand{}, "", http.StatusOK)
	if receipt := managerJSON[managerReceipt](t, receiptResponse.body); receipt.Kind != profileCommandKind ||
		receipt.State != "completed" || receipt.OperationID != nil {
		t.Fatalf("invalid profile receipt: %s", receiptResponse.body)
	}
	managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(), foreign, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, second.private+cmd.receiptPath(), revoked, managerTestCommand{}, "", http.StatusForbidden)
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, revoked, cmd, patch, http.StatusForbidden)
	_, err = owner.Exec(ctx, `DELETE FROM platform_lifecycle.scope WHERE integration_id=$1 AND amo_account_id=2542`, pilot.otherIntegration)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(), writer, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, cmd, patch, http.StatusNotFound)
}
