package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
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

	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	revoked := managerTestActor{subject: writer.subject, version: 2, grants: []string{"clients.read"}}
	foreign := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	accountPath := managerPath + "accounts/" + account.String()
	profilePath := accountPath + "/profile"
	unobserved := managerRequest(t, manager, http.MethodGet, first.private+accountPath, writer, managerTestCommand{}, "", http.StatusOK)
	if bytes.Contains(unobserved.body, []byte(`"identity_observed_at"`)) {
		t.Fatalf("identity observation time appeared without an observed identity: %s", unobserved.body)
	}
	observedAt := time.Date(2026, time.October, 1, 9, 0, 0, 0, time.UTC)
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_manager.account_identities(account_id,current_domain,display_name,observed_at)
		VALUES($1,'profile.amocrm.ru','Observed account',$2)`, account, observedAt)
	mustManager(t, err)
	start := managerRequest(t, manager, http.MethodGet, first.private+accountPath, writer, managerTestCommand{}, "", http.StatusOK)
	if start.etag != `"1"` || !bytes.Contains(start.body, []byte(`"display_name":"Observed account"`)) {
		t.Fatalf("initial profile did not reflect the observed account: %s", start.body)
	}
	var observation struct {
		IdentityObservedAt time.Time `json:"identity_observed_at"`
	}
	mustManager(t, json.Unmarshal(start.body, &observation))
	if !observation.IdentityObservedAt.Equal(observedAt) {
		t.Fatalf("identity observation time = %s; want %s", observation.IdentityObservedAt, observedAt)
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
		`{"classification":"Неизвестный статус","reason":"unsupported classification"}`, http.StatusUnprocessableEntity).
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
	listPath := managerPath + "accounts"
	list := managerRequest(t, manager, http.MethodGet, first.private+listPath+"?limit=1",
		writer, managerTestCommand{}, "", http.StatusOK)
	before := managerJSON[accountPage](t, list.body)
	if before.Next == nil || before.Summary.Overview["all"].Total != 2 ||
		before.Summary.Overview["all"].ByClassification["Без партнёра"] != 1 {
		t.Fatalf("account overview is not based on the full snapshot: %s", list.body)
	}
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, newManagerTestCommand(account, 3),
		`{"classification":"Клиент партнёра","reason":"missing partner"}`, http.StatusUnprocessableEntity).
		requireCode(t, "validation_failed")
	partner := managerRequest(t, manager, http.MethodPatch, second.private+profilePath,
		writer, newManagerTestCommand(account, 3),
		`{"classification":"Клиент партнёра","partner_name":" Fixture Partner ","reason":"confirmed by manager"}`, http.StatusOK)
	if partner.etag != `"4"` || !bytes.Contains(partner.body, []byte(`"partner_name":"Fixture Partner"`)) {
		t.Fatalf("partner assignment was not returned: %s", partner.body)
	}

	continued := managerRequest(t, manager, http.MethodGet, second.private+listPath+"?limit=1&cursor="+url.QueryEscape(*before.Next),
		writer, managerTestCommand{}, "", http.StatusOK)
	next := managerJSON[accountPage](t, continued.body)
	if !reflect.DeepEqual(next.Summary, before.Summary) {
		t.Fatalf("summary drifted between cursor pages: %s / %s", list.body, continued.body)
	}
	filtered := managerRequest(t, manager, http.MethodGet,
		first.private+listPath+"?renewal_channel=partner&data_group=no_data&q=fixture+partner&tariff=trial",
		writer, managerTestCommand{}, "", http.StatusOK)
	matched := managerJSON[accountPage](t, filtered.body)
	if matched.Summary.Total != 1 || matched.Summary.Overview["all"].Total != 2 ||
		matched.Summary.Overview["no_data"].ByClassification["Клиент партнёра"] != 1 ||
		!bytes.Contains(matched.Items[0], []byte(`"renewal_channel":"partner"`)) {
		t.Fatalf("partner and tariff filters disagreed with the overview: %s", filtered.body)
	}
	free := managerRequest(t, manager, http.MethodPatch, first.private+profilePath,
		writer, newManagerTestCommand(account, 4), `{"classification":"Без партнёра","reason":"free again"}`, http.StatusOK)
	if !bytes.Contains(free.body, []byte(`"partner_name":null`)) {
		t.Fatalf("partner was retained after change to free: %s", free.body)
	}
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath,
		writer, newManagerTestCommand(account, 5),
		`{"classification":"Клиент партнёра","partner_name":"Fixture Partner","reason":"review status"}`, http.StatusOK)
	own := managerRequest(t, manager, http.MethodPatch, first.private+profilePath,
		writer, newManagerTestCommand(account, 6),
		`{"classification":"Наш клиент","reason":"known returning client"}`, http.StatusOK)
	if own.etag != `"7"` || !bytes.Contains(own.body, []byte(`"classification":"Наш клиент"`)) ||
		!bytes.Contains(own.body, []byte(`"partner_name":null`)) {
		t.Fatalf("our client assignment did not replace the partner assignment: %s", own.body)
	}
	q := url.Values{"classification": {"Наш клиент"}}
	ownList := managerRequest(t, manager, http.MethodGet, second.private+listPath+"?"+q.Encode(),
		writer, managerTestCommand{}, "", http.StatusOK)
	ownPage := managerJSON[accountPage](t, ownList.body)
	if ownPage.Summary.Total != 1 || len(ownPage.Items) != 1 ||
		ownPage.Summary.Overview["all"].ByClassification["Наш клиент"] != 1 ||
		bytes.Contains(ownPage.Items[0], []byte(`"renewal_channel"`)) {
		t.Fatalf("our client assignment did not appear in the list and overview as expected: %s", ownList.body)
	}
	_, err = owner.Exec(ctx, `DELETE FROM platform_lifecycle.scope WHERE integration_id=$1 AND amo_account_id=2542`, pilot.otherIntegration)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(), writer, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodPatch, second.private+profilePath, writer, cmd, patch, http.StatusNotFound)
}
