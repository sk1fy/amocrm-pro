package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformFeedAcrossReplicas(t *testing.T) {
	for _, name := range []string{"PLATFORM_TEST_BINARY", "PLATFORM_TEST_CERTS_DIR", "PLATFORM_TEST_OWNER_DSN", "PLATFORM_TEST_JOBS_DSN", "PLATFORM_TEST_ACCESS_DSN"} {
		if os.Getenv(name) == "" {
			t.Skip("run make platform-test for Platform manager process checks")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	mustManager(t, err)
	defer owner.Close()
	pilot := managerPilot{ctx: ctx, owner: owner}
	pilot.fixture(t)
	jobs, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_JOBS_DSN"), storage.Jobs, 1)
	mustManager(t, err)
	mustManager(t, storage.CheckManager(ctx, jobs))
	_, err = owner.Exec(ctx, `REVOKE SELECT ON platform_manager.business_events FROM platform_jobs`)
	mustManager(t, err)
	if err = storage.CheckManager(ctx, jobs); err == nil || err.Error() != "manager read schema is unavailable" {
		t.Fatalf("missing event read grants did not fail startup check: %v", err)
	}
	_, err = owner.Exec(ctx, `GRANT SELECT ON platform_manager.business_events TO platform_jobs;REVOKE INSERT ON platform_manager.feed_snapshot_items FROM platform_jobs`)
	mustManager(t, err)
	if err = storage.CheckManager(ctx, jobs); err == nil || err.Error() != "manager mutation grants are unavailable" {
		t.Fatalf("missing snapshot grants did not fail startup check: %v", err)
	}
	_, err = owner.Exec(ctx, `GRANT INSERT ON platform_manager.feed_snapshot_items TO platform_jobs`)
	mustManager(t, err)
	mustManager(t, storage.CheckManager(ctx, jobs))
	jobs.Close()

	_, err = owner.Exec(ctx, `TRUNCATE platform_manager.feed_snapshots CASCADE`)
	mustManager(t, err)
	first, second := startManagerProcess(t), startManagerProcess(t)
	manager, core := tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "manager-bff"), tlsClient(t, os.Getenv("PLATFORM_TEST_CERTS_DIR"), "core")
	pilot.deliver(t, core, first.private)
	all := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"feed.read", "clients.read", "clients.write", "clients.renew"}}
	feedOnly := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"feed.read"}}
	clientsOnly := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read"}}
	none := managerTestCommand{}
	get := func(base, path string, actor managerTestActor) eventPage {
		return managerJSON[eventPage](t, managerRequest(t, manager, http.MethodGet, base+managerPath+path, actor, none, "", http.StatusOK).body)
	}
	initial := get(first.private, "feed", all)
	if len(initial.Items) != 5 || initial.Next != nil {
		t.Fatalf("only five applied lifecycle facts expected: %+v", initial)
	}
	account := uuid.Nil
	var episode uuid.UUID
	for _, e := range initial.Items {
		if e.Product != nil && *e.Product == pilot.product {
			account = e.Account
			for _, ref := range e.Related {
				if ref.Kind == "installation" {
					episode = ref.ID
				}
				if ref.Kind == "product" {
					t.Fatal("unconfigured catalog exposed product destination")
				}
			}
		}
	}
	if account == uuid.Nil || episode == uuid.Nil {
		t.Fatal("missing lifecycle account or episode")
	}
	history := "accounts/" + account.String() + "/history"
	catalogActor := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "catalog.manage"}}
	catalogBody := fmt.Sprintf(`{"code":"pilot-review","display_name":"Reviewed product","line":"PRO","availability":"pilot","integration_refs":[%q],"tariff_version":null,"trial_policy":{"kind":"once","duration_days":7},"links":[]}`, pilot.integration)
	managerRequest(t, manager, http.MethodPost, first.private+managerPath+"products", catalogActor, newManagerTestCommand(uuid.Nil, 0), catalogBody, http.StatusCreated)
	configured := get(first.private, "feed?product_id="+pilot.product.String(), all)
	for _, e := range configured.Items {
		if !slices.ContainsFunc(e.Related, func(ref eventReference) bool { return ref.Kind == "product" && ref.ID == pilot.product }) {
			t.Fatal("configured catalog destination missing")
		}
	}

	filtered := get(second.private, "feed?account_id="+account.String(), all)
	historical := get(first.private, history, clientsOnly)
	if len(filtered.Items) != 4 || !slices.EqualFunc(filtered.Items, historical.Items, func(a, b businessEvent) bool { return a.ID == b.ID }) {
		t.Fatal("history and feed do not share source identity")
	}
	for _, e := range get(second.private, "feed", feedOnly).Items {
		if len(e.Related) != 0 || e.Account == uuid.Nil {
			t.Fatal("feed-only scope or reference permission lost")
		}
	}
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+"feed", clientsOnly, none, "", http.StatusForbidden)
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+history, feedOnly, none, "", http.StatusForbidden)
	day := pilot.opened.In(pilot.zone).Format(time.DateOnly)
	if got := get(first.private, "feed?from="+day+"&to="+day, all); len(got.Items) != 2 {
		t.Fatalf("local date boundary: %+v", got.Items)
	}
	if got := get(first.private, "feed?kind=activated", all); len(got.Items) != 0 || got.Items == nil {
		t.Fatal("unconfirmed activation became a fact")
	}
	repeated := get(first.private, "feed?kind=installed&kind=reinstalled&kind=installed", all)
	canonical := get(first.private, "feed?kind=reinstalled&kind=installed", all)
	if len(repeated.Items) != 3 || repeated.QueryHash != canonical.QueryHash {
		t.Fatal("repeated kinds not normalized")
	}
	for _, q := range []string{"kind=", "kind=installed,reinstalled", "kind=authorization_changed", "kind=bogus", "limit=1&limit=2", "account_id=", "unknown=x", "from=2026-11-1", "from=2026-11-02&to=2026-11-01", "kind=%ff"} {
		managerRequest(t, manager, http.MethodGet, first.private+managerPath+"feed?"+q, all, none, "", http.StatusBadRequest).requireCode(t, "validation_failed")
	}
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+history+"?account_id="+account.String(), all, none, "", http.StatusBadRequest)

	review := newManagerTestCommand(episode, 1)
	// The chosen episode can be closed; both generations retain their own review version.
	var version int64
	mustManager(t, owner.QueryRow(ctx, `SELECT version FROM platform_lifecycle.episodes WHERE id=$1`, episode).Scan(&version))
	review = newManagerTestCommand(episode, version)
	const privateReason = `{"reviewed":true,"reason":"private review reason"}`
	managerRequest(t, manager, http.MethodPut, first.private+review.reviewPath(), all, review, privateReason, http.StatusOK)
	managerRequest(t, manager, http.MethodPut, second.private+review.reviewPath(), all, review, privateReason, http.StatusOK)
	profile := newManagerTestCommand(account, 1)
	managerRequest(t, manager, http.MethodPatch, first.private+managerPath+"accounts/"+account.String()+"/profile", all, profile, `{"note":"private profile note","display_name":"Private Name","reason":"private profile reason"}`, http.StatusOK)
	audit := newManagerTestCommand(account, 1)
	managerRequest(t, manager, http.MethodPost, first.private+managerPath+"accounts/"+account.String()+"/portrait/audit-dates", all, audit, `{"date":"2026-09-13","note":"private audit note"}`, http.StatusOK)
	renewal := newManagerTestCommand(account, 1)
	renewalBody := fmt.Sprintf(`{"product_id":%q,"target_until":%q,"reason":"private renewal reason"}`, pilot.product.String(), time.Now().Add(365*24*time.Hour).UTC().Format(time.RFC3339))
	renewed := managerRequest(t, manager, http.MethodPost, first.private+managerPath+"accounts/"+account.String()+"/renewals", all, renewal, renewalBody, http.StatusCreated)
	var renewalID uuid.UUID
	mustManager(t, json.Unmarshal(renewed.body, &struct {
		ID *uuid.UUID `json:"id"`
	}{ID: &renewalID}))
	facts := get(second.private, "feed?account_id="+account.String(), all)
	if len(facts.Items) != 8 {
		t.Fatalf("committed changes not represented once: %+v", facts)
	}
	for _, e := range facts.Items {
		if e.Kind == "access_renewed" && e.ID != renewalID {
			t.Fatal("renewal identity changed")
		}
	}
	raw, _ := json.Marshal(facts)
	for _, secret := range []string{"private", "Private Name", "actor_id", "command_id", "details", "source", "reason"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("private event field escaped: %s", secret)
		}
	}
	historyFacts := get(first.private, history, clientsOnly)
	if !slices.EqualFunc(facts.Items, historyFacts.Items, func(a, b businessEvent) bool { return a.ID == b.ID }) {
		t.Fatal("mutation event identities differ across endpoints")
	}
	if got := get(first.private, "feed?product_id="+pilot.product.String(), all); len(got.Items) != 6 {
		t.Fatalf("accountwide events included by literal product filter: %d", len(got.Items))
	}

	var cached int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_manager.feed_snapshots`).Scan(&cached))
	if cached != 0 {
		t.Fatal("complete one-page reads consumed cache admission")
	}
	sourceRows, err := owner.Query(ctx, `SELECT id FROM platform_manager.business_events`)
	mustManager(t, err)
	sourceIDs := make(map[uuid.UUID]bool)
	for sourceRows.Next() {
		var id uuid.UUID
		mustManager(t, sourceRows.Scan(&id))
		sourceIDs[id] = true
	}
	mustManager(t, sourceRows.Err())
	sourceRows.Close()
	for _, e := range get(first.private, "feed", all).Items {
		if !sourceIDs[e.ID] {
			t.Fatal("public ID is not durable source ID")
		}
	}
	frozen := get(first.private, "feed?limit=1", all)
	if frozen.Next == nil {
		t.Fatal("missing membership cursor")
	}
	var held wire.Event
	held.ID, held.Version, held.Kind, held.Installation, held.Integration, held.Account = uuid.New(), 1, wire.Reauthorized, pilot.otherInstallation, pilot.otherIntegration, "2542"
	held.Sequence, held.Generation, held.OccurredAt = 3, 1, pilot.opened.Add(-time.Hour)
	held.Evidence = wire.Evidence{ID: uuid.New(), Source: "core_oauth"}
	deliver := func(event wire.Event) {
		body, _ := json.Marshal(event)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, first.private+coreEventsPath, bytes.NewReader(body))
		mustManager(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := core.Do(req)
		mustManager(t, err)
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		mustManager(t, err)
		if res.StatusCode != http.StatusAccepted {
			t.Fatalf("lifecycle deliver %d: %s", res.StatusCode, raw)
		}
	}
	deliver(held)
	beforeGap := get(first.private, "feed", all)
	if len(beforeGap.Items) != 9 {
		t.Fatal("held inbox became visible")
	}
	predecessor := held
	predecessor.ID, predecessor.Sequence = uuid.New(), 2
	deliver(predecessor)
	var applied int
	mustManager(t, owner.QueryRow(ctx, `SELECT count(*) FROM platform_lifecycle.history WHERE event_id=$1`, held.ID).Scan(&applied))
	if applied != 1 {
		t.Fatal("late gap not drained")
	}
	ids := []uuid.UUID{frozen.Items[0].ID}
	cursor := *frozen.Next
	for cursor != "" {
		p := get(second.private, "feed?limit=1&cursor="+url.QueryEscape(cursor), all)
		if p.SnapshotAt != frozen.SnapshotAt {
			t.Fatal("snapshot changed across replica")
		}
		ids = append(ids, p.Items[0].ID)
		cursor = ""
		if p.Next != nil {
			cursor = *p.Next
		}
	}
	if len(ids) != 9 || slices.Contains(ids, held.ID) || slices.Contains(ids, predecessor.ID) {
		t.Fatal("late facts altered frozen membership")
	}
	fresh := get(second.private, "feed", all)
	if len(fresh.Items) != 11 {
		t.Fatal("late historical facts missing in new search")
	}
	first.stop(t)
	first = startManagerProcess(t)
	replayed := get(first.private, "feed?limit=1&cursor="+url.QueryEscape(*frozen.Next), all)
	if replayed.SnapshotAt != frozen.SnapshotAt || replayed.Items[0].ID != ids[1] {
		t.Fatal("snapshot did not survive process restart")
	}
	if got := get(first.private, "feed", all); len(got.Items) != len(fresh.Items) {
		t.Fatal("facts lost on restart")
	}

	frozen = get(first.private, "feed?limit=1", all)
	continuation := "feed?limit=1&cursor=" + url.QueryEscape(*frozen.Next)
	currentFeedOnly := feedOnly
	currentFeedOnly.subject = all.subject
	if p := get(second.private, continuation, currentFeedOnly); len(p.Items[0].Related) != 0 {
		t.Fatal("continuation reused old destination grants")
	}

	for _, path := range []string{history + "?limit=1&cursor=" + url.QueryEscape(*frozen.Next), continuation + "&kind=installed", continuation + "&account_id=" + account.String()} {
		managerRequest(t, manager, http.MethodGet, second.private+managerPath+path, all, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	}
	changed := all
	changed.version++
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+continuation, changed, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	_, err = owner.Exec(ctx, `UPDATE platform_manager.feed_snapshots SET expires_at=now()-interval '1 second' WHERE subject_id=$1`, all.subject)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+continuation, all, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")

	// Missing frozen facts invalidate the cursor rather than silently shortening a page.
	frozen = get(first.private, "feed?limit=1", all)
	_, err = owner.Exec(ctx, `DELETE FROM platform_lifecycle.history WHERE event_id=$1`, held.ID)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"feed?limit=1&cursor="+url.QueryEscape(*frozen.Next), all, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	var lifecycleID, profileID uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT id FROM platform_manager.account_change_events WHERE command_id=$1`, profile.id).Scan(&profileID))
	mustManager(t, owner.QueryRow(ctx, `SELECT event_id FROM platform_lifecycle.history LIMIT 1`).Scan(&lifecycleID))
	_, err = owner.Exec(ctx, `DELETE FROM platform_manager.account_event_outbox WHERE event_id=(SELECT id FROM platform_manager.account_change_events WHERE command_id=$1)`, profile.id)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `UPDATE platform_manager.account_change_events SET id=$1 WHERE command_id=$2`, lifecycleID, profile.id)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+"feed", all, none, "", http.StatusServiceUnavailable)
	// Removing an approved account mapping is checked on every explicit account read.
	_, err = owner.Exec(ctx, `DELETE FROM platform_lifecycle.scope WHERE integration_id=$1`, pilot.integration)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+history, clientsOnly, none, "", http.StatusNotFound)
	managerRequest(t, manager, http.MethodGet, first.private+managerPath+"feed?account_id="+account.String(), feedOnly, none, "", http.StatusNotFound)
	// Keep the account visible through another product while the original mapping becomes ambiguous.
	_, err = owner.Exec(ctx, `UPDATE platform_manager.account_change_events SET id=$1 WHERE command_id=$2`, profileID, profile.id)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,9007199254740993),($2,9007199254740993)`, pilot.integration, pilot.otherIntegration)
	mustManager(t, err)
	secondProduct := wire.Event{ID: uuid.New(), Version: 1, Kind: wire.Installed, Installation: uuid.New(), Integration: pilot.otherIntegration, Account: "9007199254740993", Sequence: 1, Generation: 1, OccurredAt: pilot.opened, Evidence: wire.Evidence{ID: uuid.New(), Source: "core_oauth"}}
	deliver(secondProduct)
	frozen = get(first.private, "feed?limit=1", all)
	ambiguous := uuid.New()
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.integrations(id,product_id) VALUES($1,$2);`, ambiguous, pilot.product)
	mustManager(t, err)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,9007199254740993)`, ambiguous)
	mustManager(t, err)
	managerRequest(t, manager, http.MethodGet, second.private+managerPath+"feed?limit=1&cursor="+url.QueryEscape(*frozen.Next), all, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	if page := get(first.private, "feed?product_id="+pilot.product.String(), all); len(page.Items) != 0 {
		t.Fatal("ambiguous product membership leaked facts")
	}
	if page := get(first.private, history, clientsOnly); len(page.Items) != 3 {
		t.Fatalf("accountwide facts or remaining membership lost: %+v", page)
	}

	// Rolling back only P09 removes derived reads and cache, preserving every source fact.
	tx, err := owner.Begin(ctx)
	mustManager(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	sourceCount := `SELECT (SELECT count(*) FROM platform_lifecycle.history)+(SELECT count(*) FROM platform_manager.review_events)+(SELECT count(*) FROM platform_manager.account_change_events)`
	var beforeDown, afterDown int64
	mustManager(t, tx.QueryRow(ctx, sourceCount).Scan(&beforeDown))
	down, err := os.ReadFile("../../migrations-platform/000010_manager_feed.down.sql")
	mustManager(t, err)
	_, err = tx.Exec(ctx, string(down))
	mustManager(t, err)
	mustManager(t, tx.QueryRow(ctx, sourceCount).Scan(&afterDown))
	if beforeDown != afterDown {
		t.Fatal("P09 down removed durable source facts")
	}
	mustManager(t, tx.Rollback(ctx))

}
