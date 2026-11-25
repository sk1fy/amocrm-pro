package productplatform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformAccountSubscription(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_BINARY") == "" {
		t.Skip("run make platform-test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	access, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_ACCESS_DSN"), storage.Access, 1)
	mustManager(t, err)
	defer access.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	keys, private := recoveryAccessKeys(t)
	proc := startManagerProcess(t, "PLATFORM_ACCESS_PROOF_PUBLIC_KEYS="+keys, "PLATFORM_ACCESS_PILOT_ORIGIN=https://fixture.amocrm.test")
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	manager := tlsClient(t, certs, "manager-bff")
	pilot.deliver(t, tlsClient(t, certs, "core"), proc.private)
	scope := platformaccess.Scope{Account: "2542", Product: "other-review", Integration: pilot.otherIntegration, Installation: pilot.otherInstallation, Generation: 1, AuthorizationSequence: 1}
	var account, product uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id,product_id FROM platform_manager.account_memberships WHERE amo_account_id=2542`).Scan(&account, &product))
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET line='TOP' WHERE id=$1`, product)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.terms(account_id,product_id,source,valid_until) VALUES($1,$2,'own_paid',now()+interval '5 days')`, account, product)
	mustManager(t, err)
	actor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.renew"}}
	path := proc.private + managerPath + "accounts/" + account.String() + "/subscription"
	until := time.Now().AddDate(1, 0, 0).Format(time.DateOnly)
	body := fmt.Sprintf(`{"action":"grant","plan":"top","seats":2,"deal_id":"123","ends_on":"%s"}`, until)
	cmd := newManagerTestCommand(account, 1)
	request := func(version int64, body string) {
		managerRequest(t, manager, http.MethodPost, path, actor, newManagerTestCommand(account, version), body, http.StatusOK)
	}
	managerRequest(t, manager, http.MethodPost, path, managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.write"}}, cmd, body, http.StatusForbidden)
	reply := managerRequest(t, manager, http.MethodPost, path, actor, cmd, body, http.StatusOK)
	var result struct {
		Classification string
		Subscription   struct{ State string }
		Change         struct {
			Source  string
			Current string
		} `json:"classification_change"`
	}
	mustManager(t, json.Unmarshal(reply.body, &result))
	if result.Classification != "Наш клиент" || result.Subscription.State != "awaiting_users" || result.Change.Source != "subscription" || result.Change.Current != "Наш клиент" {
		t.Fatalf("unexpected grant: %s", reply.body)
	}
	managerRequest(t, manager, http.MethodPost, path, actor, cmd, body, http.StatusOK)
	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.write"}}
	profile := proc.private + managerPath + "accounts/" + account.String() + "/profile"
	const freeClient = `{"classification":"Свободный клиент","reason":"Изменение статуса клиента"}`
	assertLocked := func(version int64) {
		t.Helper()
		managerRequest(t, manager, http.MethodPatch, profile, writer, newManagerTestCommand(account, version),
			freeClient, http.StatusUnprocessableEntity).requireCode(t, "classification_unavailable")
	}
	assertLocked(2)
	roster := func(n int) {
		_, err := owner.Exec(ctx, `INSERT INTO platform_manager.account_rosters(account_id,roster,active_count,evidence_id,observed_at,fresh_until)
 VALUES($1,(SELECT jsonb_agg(i) FROM generate_series(1,$2::integer) i),$2,$3,now(),now()+interval '1 day')
 ON CONFLICT(account_id) DO UPDATE SET roster=EXCLUDED.roster,active_count=EXCLUDED.active_count,observed_at=EXCLUDED.observed_at,fresh_until=EXCLUDED.fresh_until,failure_code=NULL`, account, n, uuid.New())
		mustManager(t, err)
	}
	check := func(state string, covered bool) {
		t.Helper()
		var actual string
		mustManager(t, owner.QueryRow(ctx, `SELECT state FROM platform_manager.account_subscriptions WHERE account_id=$1`, account).Scan(&actual))
		snapshot := recoveryPublicAccess(t, ctx, proc.public, private, scope)
		if actual != state || (snapshot.Source == "subscription") != covered || snapshot.OwnUntil == nil || !snapshot.Active {
			t.Fatalf("state %s access %+v", actual, snapshot)
		}
	}
	roster(2)
	check("active", true)
	// A TOP product added after the grant inherits coverage when it is installed.
	added := uuid.New()
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.products(id,code,line) VALUES($1,'subsequent-top','TOP')`, added)
	mustManager(t, err)
	var covered bool
	mustManager(t, access.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_access.subscription_terms WHERE account_id=$1 AND product_id=$2)`, account, added).Scan(&covered))
	if !covered {
		t.Fatal("new TOP product is missing from existing subscription")
	}

	_, err = owner.Exec(ctx, `UPDATE platform_manager.account_rosters SET failure_code='source_timeout' WHERE account_id=$1`, account)
	mustManager(t, err)
	check("active", true)
	roster(3)
	check("suspended", false)
	assertLocked(2)
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()-interval '1 day' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)
	if access := recoveryPublicAccess(t, ctx, proc.public, private, scope); access.Active {
		t.Fatalf("suspended package still grants access: %+v", access)
	}
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()+interval '5 days' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)

	request(2, `{"action":"seats","seats":3}`)
	check("active", true)
	roster(4)
	check("suspended", false)
	roster(3)
	check("active", true)
	request(3, fmt.Sprintf(`{"action":"renew","plan":"top","seats":3,"deal_id":"456","ends_on":"%s"}`, time.Now().AddDate(2, 0, 0).Format(time.DateOnly)))
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET line='PRO' WHERE id=$1`, product)
	mustManager(t, err)
	check("active", false)
	request(4, `{"action":"revoke"}`)
	check("revoked", false)
	request(5, fmt.Sprintf(`{"action":"grant","plan":"unified","seats":3,"deal_id":"789","ends_on":"%s"}`, until))
	check("active", true)
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()+interval '3 years' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)
	check("active", false)
	// Even when the individual term wins the expiry calculation, the package covers PRO.
	managerRequest(t, manager, http.MethodPost, proc.private+managerPath+"accounts/"+account.String()+"/renewals", actor,
		newManagerTestCommand(account, 1), renewalTestBody(product, time.Now().AddDate(4, 0, 0), "covered product"), http.StatusUnprocessableEntity).requireCode(t, "renewal_unavailable")

	updated := managerRequest(t, manager, http.MethodPatch, profile, writer, newManagerTestCommand(account, 6), freeClient, http.StatusOK)
	mustManager(t, json.Unmarshal(updated.body, &result))
	if result.Classification != "Свободный клиент" || result.Change.Source != "manual" || result.Change.Current != "Свободный клиент" {
		t.Fatalf("expected manual status change under unified subscription: %s", updated.body)
	}

	request(7, `{"action":"revoke"}`)
	request(8, body)
	_, err = owner.Exec(ctx, `UPDATE platform_manager.account_subscription_revisions
		SET ends_on=(statement_timestamp() AT TIME ZONE 'Europe/Moscow')::date - 1 WHERE account_id=$1 AND version=9`, account)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodPatch, profile, writer, newManagerTestCommand(account, 9), freeClient, http.StatusOK)
	reactivated := managerRequest(t, manager, http.MethodPost, path, actor, newManagerTestCommand(account, 10),
		fmt.Sprintf(`{"action":"renew","plan":"top","seats":3,"deal_id":"902","ends_on":"%s"}`, until), http.StatusOK)
	mustManager(t, json.Unmarshal(reactivated.body, &result))
	if result.Classification != "Наш клиент" || result.Change.Source != "subscription" || result.Change.Current != "Наш клиент" {
		t.Fatalf("expected reactivated TOP subscription to restore status: %s", reactivated.body)
	}
	assertLocked(11)

}
