package productplatform

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
)

func TestPlatformPortraitAcrossReplicas(t *testing.T) {
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
	_, err = owner.Exec(ctx, `
		TRUNCATE platform_manager.portrait_niches CASCADE;
		UPDATE platform_manager.portrait_template SET version=1,topics='[]';
		UPDATE platform_manager.portrait_niche_revision SET version=1`)
	mustManager(t, err)
	first, second := startManagerProcess(t), startManagerProcess(t)
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	manager := tlsClient(t, certs, "manager-bff")
	manager.Timeout = 5 * time.Second
	pilot.deliver(t, tlsClient(t, certs, "core"), first.private)

	var accountA, accountB uuid.UUID
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=2542`).Scan(&accountA))
	mustManager(t, owner.QueryRow(ctx, `SELECT account_id FROM platform_manager.visible_accounts WHERE amo_account_id=9007199254740993`).Scan(&accountB))
	portraitA := managerPath + "accounts/" + accountA.String() + "/portrait"
	portraitB := managerPath + "accounts/" + accountB.String() + "/portrait"
	templatePath := managerPath + "portrait-template"
	nichePath := managerPath + "portrait-niches"
	writer := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "clients.write"}}
	config := managerTestActor{subject: uuid.New(), version: 1, grants: []string{"clients.read", "configuration.manage"}}
	none := managerTestCommand{}

	initial := managerRequest(t, manager, http.MethodGet, first.private+portraitA, writer, none, "", http.StatusOK)
	if initial.etag != `"1"` || !bytes.Contains(initial.body, []byte(`"company":{}`)) ||
		!bytes.Contains(initial.body, []byte(`"topics":[]`)) {
		t.Fatalf("unconfigured account is not an empty versioned portrait: %s", initial.body)
	}
	topic1, topic2 := uuid.New(), uuid.New()
	topic := func(id uuid.UUID, name string) string {
		return fmt.Sprintf(`{"id":%q,"title":%q,"prompts":["Вопрос"]}`, id.String(), name)
	}
	template := func(a, b string) string { return fmt.Sprintf(`{"topics":[%s,%s]}`, a, b) }
	topicA, topicB := topic(topic1, "Тема 1"), topic(topic2, "Тема 2")
	putTemplate := func(actor managerTestActor, version int64, input string, status int) observedManagerResponse {
		return managerRequest(t, manager, http.MethodPut, first.private+templatePath, actor,
			newManagerTestCommand(uuid.Nil, version), input, status)
	}
	putTemplate(writer, 1, template(topicA, topicB), http.StatusForbidden)
	configured := putTemplate(config, 1, template(topicA, topicB), http.StatusOK)
	if configured.etag != `"2"` {
		t.Fatalf("initial template version %s", configured.etag)
	}
	createNiche := func(name string) portraitNiche {
		res := managerRequest(t, manager, http.MethodPost, first.private+nichePath, config, newManagerTestCommand(uuid.Nil, 0),
			fmt.Sprintf(`{"name":%q}`, name), http.StatusCreated)
		return managerJSON[portraitNiche](t, res.body)
	}
	source, target, temporary := createNiche("Автомобили"), createNiche("Логистика"), createNiche("Производство")
	niches := managerJSON[portraitNichePage](t, managerRequest(t, manager, http.MethodGet, first.private+nichePath+"?limit=1",
		writer, none, "", http.StatusOK).body)
	if len(niches.Items) != 1 || niches.Next == nil {
		t.Fatalf("niches were not paginated: %+v", niches)
	}

	signal := portraitFieldID("maturity/tasks")
	updateA := fmt.Sprintf(`{"company":{"niche_id":%q,"rating":5,"work_types":[],"site":"https://example.test"},
		"maturity_signals":[{"id":%q,"manual":false}],"topics":[{"id":%q,"note":"Первый"},{"id":%q,"note":"Второй"}]}`,
		source.ID.String(), signal.String(), topic1.String(), topic2.String())
	editA := newManagerTestCommand(accountA, 1)
	applied := managerRequest(t, manager, http.MethodPatch, first.private+portraitA, writer, editA, updateA, http.StatusOK)
	if applied.etag != `"2"` || !bytes.Contains(applied.body, []byte(`"manual":false`)) ||
		!bytes.Contains(applied.body, []byte(`"work_types":[]`)) || bytes.Contains(applied.body, []byte(`"observed"`)) {
		t.Fatalf("portrait nullable/manual fields lost: %s", applied.body)
	}
	stale := managerRequest(t, manager, http.MethodPatch, second.private+portraitA, writer, newManagerTestCommand(accountA, 1),
		`{"company":{"site":null}}`, http.StatusConflict)
	if stale.requireCode(t, "version_conflict").Version != 2 {
		t.Fatalf("stale portrait response %s", stale.body)
	}
	putTemplate(config, 2, template(topicB, topicA), http.StatusOK)
	reordered := managerJSON[accountPortrait](t, managerRequest(t, manager, http.MethodGet, second.private+portraitA, writer, none, "", http.StatusOK).body)
	if reordered.TemplateVersion != 3 || reordered.Version != 2 || len(reordered.Topics) != 2 ||
		reordered.Topics[0].ID != topic2 || reordered.Topics[0].Note != "Второй" {
		t.Fatalf("template reorder lost notes or bumped account version: %+v", reordered)
	}
	putTemplate(config, 3, fmt.Sprintf(`{"topics":[%s]}`, topicB), http.StatusOK)
	hidden := managerJSON[accountPortrait](t, managerRequest(t, manager, http.MethodGet, second.private+portraitA, writer, none, "", http.StatusOK).body)
	if len(hidden.Topics) != 1 || hidden.Topics[0].ID != topic2 {
		t.Fatalf("retired template topic remained visible: %+v", hidden)
	}
	putTemplate(config, 4, template(topicB, topicA), http.StatusOK)

	audit := newManagerTestCommand(accountA, 2)
	const auditBody = `{"date":"2026-09-13","note":"private audit note"}`
	added := managerRequest(t, manager, http.MethodPost, first.private+portraitA+"/audit-dates", writer, audit, auditBody, http.StatusOK)
	if added.etag != `"3"` {
		t.Fatalf("audit did not advance portrait: %s", added.body)
	}
	duplicate := newManagerTestCommand(accountA, 3)
	managerRequest(t, manager, http.MethodPost, second.private+portraitA+"/audit-dates", writer, duplicate,
		auditBody, http.StatusConflict).requireCode(t, "state_conflict")
	managerRequest(t, manager, http.MethodDelete, second.private+nichePath+"/"+source.ID.String(), config,
		newManagerTestCommand(source.ID, 1), "", http.StatusConflict).requireCode(t, "state_conflict")
	mergeBody := fmt.Sprintf(`{"target_id":%q,"target_version":1}`, target.ID.String())
	conflictBody := fmt.Sprintf(`{"target_id":%q,"target_version":2}`, target.ID.String())
	managerRequest(t, manager, http.MethodPost, first.private+nichePath+"/"+source.ID.String()+"/merge", config,
		newManagerTestCommand(source.ID, 1), conflictBody, http.StatusConflict).requireCode(t, "version_conflict")
	merge := newManagerTestCommand(source.ID, 1)
	merged := managerRequest(t, manager, http.MethodPost, first.private+nichePath+"/"+source.ID.String()+"/merge", config,
		merge, mergeBody, http.StatusOK)
	if merged.etag != `"2"` || managerJSON[portraitNiche](t, merged.body).ID != target.ID {
		t.Fatalf("merge did not return the updated target: %s", merged.body)
	}
	mergeReplayed := managerRequest(t, manager, http.MethodPost, second.private+nichePath+"/"+source.ID.String()+"/merge", config,
		merge, mergeBody, http.StatusOK)
	if merged.etag != mergeReplayed.etag || !bytes.Equal(merged.body, mergeReplayed.body) {
		t.Fatal("merge replay changed its response")
	}
	managerRequest(t, manager, http.MethodPatch, first.private+nichePath+"/"+source.ID.String(), config,
		newManagerTestCommand(source.ID, 2), `{"name":"Отсутствует"}`, http.StatusNotFound)
	if response := managerRequest(t, manager, http.MethodGet, second.private+merge.receiptPath(), config, none, "", http.StatusOK); managerJSON[managerReceipt](t, response.body).State != "completed" {
		t.Fatalf("merged source lost its receipt: %s", response.body)
	}
	managerRequest(t, manager, http.MethodGet, first.private+nichePath+"?limit=1&cursor="+url.QueryEscape(*niches.Next),
		writer, none, "", http.StatusBadRequest).requireCode(t, "invalid_cursor")
	deletion := newManagerTestCommand(temporary.ID, 1)
	empty := managerRequest(t, manager, http.MethodDelete, first.private+nichePath+"/"+temporary.ID.String(),
		config, deletion, "", http.StatusNoContent)
	replayedDeletion := managerRequest(t, manager, http.MethodDelete, second.private+nichePath+"/"+temporary.ID.String(),
		config, deletion, "", http.StatusNoContent)
	if len(empty.body) != 0 || len(replayedDeletion.body) != 0 || empty.etag != "" {
		t.Fatal("204 deletion returned a body or ETag")
	}

	managerRequest(t, manager, http.MethodPatch, first.private+portraitB, writer, newManagerTestCommand(accountB, 1),
		`{"company":{"rating":4,"customer_types":["B2B"]}}`, http.StatusOK)
	searchPath := managerPath + "accounts/search?limit=1"
	condition := func(id uuid.UUID, op, value string) string {
		return fmt.Sprintf(`{"condition":{"field_id":%q,"op":%q,"value":%s}}`, id.String(), op, value)
	}
	rating := condition(portraitFieldID("rating"), "gte", "4")
	b2b := condition(portraitFieldID("customer_types"), "in", `["B2B","B2C"]`)
	manual := condition(signal, "eq", "false")
	query := fmt.Sprintf(`{"portrait_filter":{"and":[%s,{"or":[%s,%s]}]}}`, rating, b2b, manual)
	search := func(base, cursor, query string, status int) observedManagerResponse {
		path := base + searchPath
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		return managerRequest(t, manager, http.MethodPost, path, writer, newManagerTestCommand(uuid.Nil, 0), query, status)
	}
	filtered := managerJSON[accountPage](t, search(first.private, "", query, http.StatusOK).body)
	if filtered.Summary.Total != 2 || filtered.Next == nil || len(filtered.Items) != 1 {
		t.Fatalf("nested portrait filter did not include both accounts: %+v", filtered)
	}
	managerRequest(t, manager, http.MethodPatch, second.private+portraitB, writer, newManagerTestCommand(accountB, 2),
		`{"company":{"rating":null}}`, http.StatusOK)
	continued := managerJSON[accountPage](t, search(second.private, *filtered.Next, query, http.StatusOK).body)
	if continued.Summary.Total != 2 || continued.Next != nil || !continued.SnapshotAt.Equal(filtered.SnapshotAt) ||
		len(continued.Items) != 1 || bytes.Equal(continued.Items[0], filtered.Items[0]) {
		t.Fatalf("portrait changes displaced the frozen second page: %+v", continued)
	}
	fresh := managerJSON[accountPage](t, search(second.private, "", query, http.StatusOK).body)
	if fresh.Summary.Total != 1 || fresh.Next != nil {
		t.Fatalf("new search ignored the cleared rating: %+v", fresh)
	}
	mergedTopicSearch := fmt.Sprintf(`{"portrait_filter":%s}`, condition(topic1, "eq", `"Первый"`))
	if topicResults := managerJSON[accountPage](t, search(second.private, "", mergedTopicSearch, http.StatusOK).body); topicResults.Summary.Total != 1 {
		t.Fatalf("restored topic did not retain its note: %+v", topicResults)
	}
	search(second.private, *filtered.Next, mergedTopicSearch, http.StatusBadRequest).requireCode(t, "invalid_cursor")
	for _, malformed := range []string{
		fmt.Sprintf(`{"portrait_filter":%s}`, condition(uuid.New(), "eq", `"unknown"`)),
		fmt.Sprintf(`{"portrait_filter":{"and":[{"or":[{"and":[%s]}]}]}}`, rating),
		fmt.Sprintf(`{"portrait_filter":{"and":[%s],"or":[%s]}}`, rating, b2b),
	} {
		search(second.private, "", malformed, http.StatusBadRequest).requireCode(t, "validation_failed")
	}

	first.stop(t)
	first = startManagerProcess(t)
	restored := managerJSON[accountPortrait](t, managerRequest(t, manager, http.MethodGet, first.private+portraitA,
		writer, none, "", http.StatusOK).body)
	if restored.Version != 4 || restored.TemplateVersion != 5 || restored.Company.NicheID == nil ||
		*restored.Company.NicheID != target.ID || len(restored.AuditDates) != 1 || len(restored.Topics) != 2 {
		t.Fatalf("portrait/audit/template did not survive replacement process: %+v", restored)
	}
	replay := managerRequest(t, manager, http.MethodPatch, first.private+portraitA, writer, editA, updateA, http.StatusOK)
	if replay.etag != applied.etag || !bytes.Equal(replay.body, applied.body) {
		t.Fatal("portrait replay changed after replacement process")
	}
	var events, safeRecords int
	mustManager(t, owner.QueryRow(ctx, `
		SELECT count(*),count(o.event_id) FILTER (WHERE o.payload->>'id'=e.id::text
			AND NOT (o.payload ?| ARRAY['details','note','actor_id']))
		FROM platform_manager.account_change_events e LEFT JOIN platform_manager.account_event_outbox o ON o.event_id=e.id
		WHERE e.account_id=$1 AND e.kind='portrait_updated'`, accountA).Scan(&events, &safeRecords))
	if events != 3 || safeRecords != 3 {
		t.Fatalf("portrait projection recorded %d events, %d with matching safe outbox IDs", events, safeRecords)
	}
}
