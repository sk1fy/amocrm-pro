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
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
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
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 1)
	mustManager(t, err)
	defer jobs.Close()
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
	publish := func(n int, at time.Time, eligible bool, outcome queue.Outcome) {
		t.Helper()
		rows := make([]accountCRMUser, n)
		for i := range rows {
			rows[i] = accountCRMUser{ID: fmt.Sprint(i + 1), Active: true}
		}
		tx, err := jobs.Begin(ctx)
		mustManager(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = publishAccountObservation(ctx, tx, accountRefreshPayload{Account: account, Component: refreshUsers},
			sourceScope{}, accountObservation{At: at, Active: n, Roster: rows}, outcome, eligible)
		mustManager(t, err)
		mustManager(t, tx.Commit(ctx))
	}
	var at time.Time
	sourceClockOffset := -20 * time.Second
	next := func() time.Time {
		t.Helper()
		var now time.Time
		mustManager(t, owner.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now))
		at = now.Add(sourceClockOffset)
		return at
	}
	roster := func(n int) {
		t.Helper()
		publish(n, next(), true, queue.Outcome{})
	}
	reader := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read"}}
	detail := proc.private + managerPath + "accounts/" + account.String()
	check := func(state string, warning, covered bool) {
		t.Helper()
		response := managerRequest(t, manager, http.MethodGet, detail, reader, managerTestCommand{}, "", http.StatusOK)
		var actual struct {
			Subscription struct {
				State   string
				Warning bool `json:"seats_warning"`
			}
		}
		mustManager(t, json.Unmarshal(response.body, &actual))
		snapshot := recoveryPublicAccess(t, ctx, proc.public, private, scope)
		if actual.Subscription.State != state || actual.Subscription.Warning != warning ||
			(snapshot.Source == "subscription") != covered || snapshot.OwnUntil == nil || !snapshot.Active {
			t.Fatalf("subscription %+v access %+v", actual.Subscription, snapshot)
		}
	}
	roster(2)
	check("active", false, true)
	// A TOP product added after the grant inherits coverage when it is installed.
	added := uuid.New()
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.products(id,code,line) VALUES($1,'subsequent-top','TOP')`, added)
	mustManager(t, err)
	var covered bool
	mustManager(t, access.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_access.subscription_terms WHERE account_id=$1 AND product_id=$2)`, account, added).Scan(&covered))
	if !covered {
		t.Fatal("new TOP product is missing from existing subscription")
	}

	roster(3)
	check("active", true, true)
	publish(3, at, true, queue.Outcome{}) // An exact replay does not add a check.
	publish(0, next(), true, queue.Outcome{FailureCode: "source_timeout"})
	publish(2, next(), false, queue.Outcome{}) // Installation-deal prefetch does not clear the warning.
	publish(2, at.Add(-25*time.Hour), true, queue.Outcome{})
	check("active", true, true)
	roster(3)
	check("suspended", false, false)
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()-interval '1 day' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)
	if access := recoveryPublicAccess(t, ctx, proc.public, private, scope); access.Active {
		t.Fatalf("suspended package still grants access: %+v", access)
	}
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()+interval '5 days' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)

	request(2, `{"action":"seats","seats":3}`)
	check("active", false, true)
	// A fresh check from a slow source clock still counts after a limit
	// change, even when its source time predates the change.
	roster(4)
	check("active", true, true)
	request(3, fmt.Sprintf(`{"action":"renew","plan":"top","seats":3,"deal_id":"456","ends_on":"%s"}`, time.Now().AddDate(2, 0, 0).Format(time.DateOnly)))
	check("active", true, true)
	roster(4)
	check("suspended", false, false)
	roster(3)
	check("active", false, true)
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.products SET line='PRO' WHERE id=$1`, product)
	mustManager(t, err)
	check("active", false, false)
	request(4, `{"action":"revoke"}`)
	check("revoked", false, false)
	request(5, fmt.Sprintf(`{"action":"grant","plan":"unified","seats":3,"deal_id":"789","ends_on":"%s"}`, until))
	check("active", false, true)
	roster(4)
	check("active", true, true)
	roster(4)
	check("suspended", false, false)
	roster(3)
	check("active", false, true)
	_, err = owner.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=now()+interval '3 years' WHERE account_id=$1 AND product_id=$2`, account, product)
	mustManager(t, err)
	check("active", false, false)
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

	// An already accepted check from a fast source clock must not count for
	// a new limit, even when its source time is later than the limit change.
	sourceClockOffset = 20 * time.Second
	roster(5)
	check("active", true, false)
	request(11, `{"action":"seats","seats":4}`)
	check("active", false, false)
	publish(5, at, true, queue.Outcome{})
	check("active", false, false)
	roster(5)
	check("active", true, false)
	roster(5)
	check("suspended", false, false)
}
