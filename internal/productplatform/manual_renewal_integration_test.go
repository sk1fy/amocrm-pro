package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformManualRenewal(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(name) == "" {
			t.Skip("run make platform-test for Platform manager process checks")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 70*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 2)
	mustManager(t, err)
	defer jobs.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	first, second := startManagerProcess(t), startManagerProcess(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	manager, core := tlsClient(t, certs, "manager-bff"), tlsClient(t, certs, "core")
	manager.Timeout, core.Timeout = 7*time.Second, 8*time.Second
	pilot.deliver(t, core, first.private)

	var account, otherAccount, otherProduct uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=9007199254740993`).Scan(&account))
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id,product_id FROM platform_manager.account_memberships WHERE amo_account_id=2542`).Scan(&otherAccount, &otherProduct))
	var expired, grantedAt time.Time
	var grantEvent uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `UPDATE platform_lifecycle.terms SET valid_until=statement_timestamp()-interval '1 day'
		WHERE account_id=$1 AND product_id=$2 RETURNING valid_until,grant_event_id,granted_at`, account, pilot.product).
		Scan(&expired, &grantEvent, &grantedAt))
	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.renew"}}
	noRenewGrant := managerTestActor{subject: writer.subject, version: 2, grants: []string{"clients.read", "clients.write"}}
	foreign := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.renew"}}
	path := managerPath + "accounts/" + account.String() + "/renewals"
	normalized := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	requested := normalized.Add(879 * time.Nanosecond)
	cmd := newManagerTestCommand(account, 1)
	body := renewalTestBody(pilot.product, requested, "Internal extension reason")

	managerRequest(t, manager, http.MethodPost, first.private+path, managerTestActor{}, cmd, body, http.StatusUnauthorized)
	managerRequest(t, manager, http.MethodPost, first.private+path, noRenewGrant, cmd, body, http.StatusForbidden)
	managerRequest(t, manager, http.MethodPost, first.private+path, writer, cmd,
		renewalTestBody(otherProduct, requested, "Wrong account-product pair"), http.StatusNotFound)
	managerRequest(t, manager, http.MethodPost, first.private+path, writer, cmd,
		renewalTestBody(pilot.product, requested, " "), http.StatusBadRequest)
	created := managerRequest(t, manager, http.MethodPost, first.private+path, writer, cmd, body, http.StatusCreated)
	result := managerJSON[accountRenewal](t, created.body)
	if created.etag != `"2"` || result.ID == uuid.Nil || result.Account != account || result.Product != pilot.product ||
		!result.TargetUntil.Equal(normalized) || result.Reason != "Internal extension reason" || !result.Access.Active ||
		result.Access.ValidUntil == nil || !result.Access.ValidUntil.Equal(normalized) || result.Access.Source != "manual" || result.Access.Version != 2 {
		t.Fatalf("unexpected renewed entitlement: %s", created.body)
	}
	first.stop(t)
	first = startManagerProcess(t)
	alternate := normalized.Add(5 * time.Nanosecond).In(pilot.zone)
	replayed := managerRequest(t, manager, http.MethodPost, first.private+path, writer, cmd,
		renewalTestBody(pilot.product, alternate, "  Internal extension reason  "), http.StatusCreated)
	if replayed.etag != created.etag || !bytes.Equal(replayed.body, created.body) {
		t.Fatalf("normalized replay changed the original response: %s / %s", created.body, replayed.body)
	}
	managerRequest(t, manager, http.MethodPost, second.private+path, writer, cmd,
		renewalTestBody(pilot.product, requested, "A different reason"), http.StatusConflict).requireCode(t, "idempotency_conflict")
	stale := newManagerTestCommand(account, 1)
	if current := managerRequest(t, manager, http.MethodPost, second.private+path, writer, stale,
		renewalTestBody(pilot.product, requested.Add(time.Hour), "Stale entitlement"), http.StatusConflict).
		requireCode(t, "version_conflict").Version; current != 2 {
		t.Fatalf("stale entitlement returned version %d", current)
	}
	managerRequest(t, manager, http.MethodPost, second.private+path, writer, newManagerTestCommand(account, 2), body,
		http.StatusUnprocessableEntity).requireCode(t, "target_not_extended")
	managerRequest(t, manager, http.MethodPost, second.private+path, writer, newManagerTestCommand(account, 2),
		renewalTestBody(pilot.product, requested.Add(-time.Hour), "Shorter target"),
		http.StatusUnprocessableEntity).requireCode(t, "target_not_extended")

	var termOK, eventOK bool
	mustManager(t, owner.QueryRow(ctx, `SELECT valid_until=$3 AND source='manual' AND reason='Internal extension reason'
		AND entitlement_version=2 AND policy_version=1 AND policy_rule='pro_7d' AND policy_evidence='P05 isolated fixture'
		AND grant_event_id=$4 AND granted_at=$5 FROM platform_lifecycle.terms WHERE account_id=$1 AND product_id=$2`,
		account, pilot.product, normalized, grantEvent, grantedAt).Scan(&termOK))
	mustManager(t, owner.QueryRow(ctx, `SELECT e.id=$2 AND e.actor_id=$3 AND e.reason='Internal extension reason' AND e.version=2
		AND (e.details#>>'{before,valid_until}')::timestamptz=$4 AND e.details#>>'{before,source}'='own_trial'
		AND (e.details#>>'{after,valid_until}')::timestamptz=$5 AND e.details#>>'{after,source}'='manual'
		AND o.payload->>'id'=e.id::text AND o.payload->>'kind'='access_renewed'
		AND NOT(o.payload ?| ARRAY['reason','details','actor_id'])
		FROM platform_manager.account_change_events e JOIN platform_manager.account_event_outbox o ON o.event_id=e.id
		WHERE e.command_id=$1`, cmd.id, result.ID, writer.subject, expired, normalized).Scan(&eventOK))
	if !termOK || !eventOK {
		t.Fatalf("renewal changed original policy evidence or wrote inconsistent event evidence: term=%v event=%v", termOK, eventOK)
	}
	receipt := managerJSON[managerReceipt](t, managerRequest(t, manager, http.MethodGet, second.private+cmd.receiptPath(),
		writer, managerTestCommand{}, "", http.StatusOK).body)
	if receipt.ID != cmd.id || receipt.Kind != renewalCommandKind || receipt.State != "completed" || receipt.OperationID != nil {
		t.Fatalf("unexpected renewal receipt: %+v", receipt)
	}
	managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(), foreign, managerTestCommand{}, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, second.private+cmd.receiptPath(), noRenewGrant, managerTestCommand{}, "", http.StatusForbidden)

	closeEvent := wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Uninstalled, Installation: pilot.installation,
		Integration: pilot.integration, Account: "9007199254740993", Sequence: 7, Generation: 2, OccurredAt: time.Now().UTC(),
		Evidence: wire.Evidence{ID: uuid.New(), Source: "core_uninstall"}}
	closeRaw, err := json.Marshal(closeEvent)
	mustManager(t, err)
	_, err = NewLifecycleStore(jobs).Record(ctx, closeRaw)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodPost, first.private+path, writer, newManagerTestCommand(account, 2),
		renewalTestBody(pilot.product, requested.Add(time.Hour), "Closed installation"),
		http.StatusUnprocessableEntity).requireCode(t, "renewal_unavailable")
	closedReplay := managerRequest(t, manager, http.MethodPost, second.private+path, writer, cmd, body, http.StatusCreated)
	if closedReplay.etag != created.etag || !bytes.Equal(closedReplay.body, created.body) {
		t.Fatalf("closed installation could not replay its recorded renewal: %s", closedReplay.body)
	}
	managerRequest(t, manager, http.MethodGet, first.private+cmd.receiptPath(), writer, managerTestCommand{}, "", http.StatusOK)

	otherPath := managerPath + "accounts/" + otherAccount.String() + "/renewals"
	missing := newManagerTestCommand(otherAccount, 1)
	missingBody := renewalTestBody(otherProduct, normalized.Add(24*time.Hour), "Issue without trial")
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET line='FREE' WHERE id=$1`, otherProduct)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodPost, first.private+otherPath, writer, newManagerTestCommand(otherAccount, 1), missingBody, http.StatusUnprocessableEntity).requireCode(t, "renewal_unavailable")
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET line='PRO' WHERE id=$1`, otherProduct)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `CREATE FUNCTION p07_fail_renewal_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'renewal outbox failure in test'; END $$;
		CREATE TRIGGER p07_fail_renewal_outbox BEFORE INSERT ON platform_manager.account_event_outbox
		FOR EACH ROW EXECUTE FUNCTION p07_fail_renewal_outbox();`)
	mustManager(t, err)
	defer func() { _, _ = owner.Exec(ctx, `DROP FUNCTION IF EXISTS p07_fail_renewal_outbox() CASCADE`) }()
	managerRequest(t, manager, http.MethodPost, first.private+otherPath, writer, missing, missingBody, http.StatusServiceUnavailable)
	managerRequest(t, manager, http.MethodGet, second.private+missing.receiptPath(), writer, managerTestCommand{}, "", http.StatusNotFound)
	var absent bool
	mustManager(t, owner.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM platform_lifecycle.terms WHERE account_id=$1)`, otherAccount).Scan(&absent))
	if !absent {
		t.Fatal("failed event write left a partially committed entitlement")
	}
	_, err = owner.Exec(ctx, `DROP FUNCTION p07_fail_renewal_outbox() CASCADE`)
	mustManager(t, err)
	competing := make(chan observedManagerResponse, 2)
	for _, base := range []string{first.private, second.private} {
		go func() {
			competing <- managerCall(manager, http.MethodPost, base+otherPath, writer, missing, missingBody)
		}()
	}
	firstReply, secondReply := <-competing, <-competing
	firstReply.requireStatus(t, http.StatusCreated)
	secondReply.requireStatus(t, http.StatusCreated)
	if firstReply.etag != `"2"` || secondReply.etag != firstReply.etag || !bytes.Equal(firstReply.body, secondReply.body) {
		t.Fatalf("concurrent retries created different results: %s / %s", firstReply.body, secondReply.body)
	}
	missingResult := managerJSON[accountRenewal](t, firstReply.body)
	var termCount, eventCount, outboxCount int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_lifecycle.terms
		WHERE account_id=$1 AND product_id=$2 AND source='manual' AND entitlement_version=2
		AND num_nonnulls(policy_version, policy_rule, policy_evidence, grant_event_id, granted_at)=0`,
		otherAccount, otherProduct).Scan(&termCount))
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*),count(o.event_id) FILTER (WHERE e.id=$2 AND e.details->'before'='null'::jsonb)
		FROM platform_manager.account_change_events e LEFT JOIN platform_manager.account_event_outbox o ON o.event_id=e.id
		WHERE e.account_id=$1`, otherAccount, missingResult.ID).Scan(&eventCount, &outboxCount))
	if termCount != 1 || eventCount != 1 || outboxCount != 1 {
		t.Fatalf("first issuance did not converge on one term/event/outbox: %d/%d/%d", termCount, eventCount, outboxCount)
	}
	var canReadReason, canChangePolicy, accessCanWrite bool
	mustManager(t, owner.QueryRow(ctx, `SELECT has_column_privilege('platform_jobs','platform_lifecycle.terms','reason','SELECT'),
		has_column_privilege('platform_jobs','platform_lifecycle.terms','policy_evidence','UPDATE'),
		has_column_privilege('platform_access','platform_lifecycle.terms','valid_until','UPDATE')`).
		Scan(&canReadReason, &canChangePolicy, &accessCanWrite))
	if canReadReason || canChangePolicy || accessCanWrite {
		t.Fatal("runtime privileges can read internal reasons, rewrite policy evidence, or let public access mutate terms")
	}
}

func renewalTestBody(product uuid.UUID, target time.Time, reason string) string {
	return fmt.Sprintf(`{"product_id":%q,"target_until":%q,"reason":%q}`, product.String(), target.Format(time.RFC3339Nano), reason)
}
