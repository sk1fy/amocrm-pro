package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/installations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

func dpRequest(t *testing.T, key string, accountID int64, eventType int, direction, groupID string) *http.Request {
	t.Helper()
	body := map[string]any{
		"event": map[string]any{
			"type": eventType, "type_code": "lead_appeared_in_status", "time": 1491300016,
			"data": map[string]any{"id": 10, "element_type": 2, "status_id": 30, "pipeline_id": 20, "direction_of_movement": direction},
		},
		"action":     map[string]any{"settings": map[string]any{"widget": map[string]any{"settings": map[string]any{"key": key, "groupId": groupID}}}},
		"subdomain":  "test",
		"account_id": accountID,
	}
	raw, _ := json.Marshal(body)
	return httptest.NewRequest(http.MethodPost, "/api/v1/widget/distribution/dp", strings.NewReader(string(raw)))
}

func dpFormBody(key string, accountID int64, groupID string, eventType int, direction string, id, statusID, pipelineID, ts int64) url.Values {
	form := url.Values{}
	form.Set("event[type]", strconv.Itoa(eventType))
	form.Set("event[type_code]", "lead_appeared_in_status")
	form.Set("event[time]", strconv.FormatInt(ts, 10))
	form.Set("event[data][id]", strconv.FormatInt(id, 10))
	form.Set("event[data][element_type]", "2")
	form.Set("event[data][status_id]", strconv.FormatInt(statusID, 10))
	form.Set("event[data][pipeline_id]", strconv.FormatInt(pipelineID, 10))
	form.Set("event[data][direction_of_movement]", direction)
	form.Set("action[settings][widget][settings][key]", key)
	form.Set("action[settings][widget][settings][groupId]", groupID)
	form.Set("subdomain", "test")
	form.Set("account_id", strconv.FormatInt(accountID, 10))
	return form
}

func dpFormRequest(form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/widget/distribution/dp", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// Fail-closed: a Digital Pipeline trigger without a usable source time must
// never be turned into "now" as entry evidence.
func TestDPTriggerWorkerNoFabricatedTime(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	installID, accountID := f.Assignment.Scope.InstallationID, f.Assignment.Scope.AccountID
	worker := &DPTriggerWorker{Store: f.Store, CRM: f.CRM}
	inboxID := uuid.New()
	dedup := sha256.Sum256([]byte("fail-closed-no-time"))
	if _, e := f.Pool.Exec(ctx, `INSERT INTO distribution_dp_inbox(id,dedup_key,installation_id,account_id,lead_id,pipeline_id,status_id,event_type,direction,group_id,occurred_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		inboxID, dedup[:], installID, accountID, int64(10), int64(20), int64(30), 15, "went_to_trigger", uuid.New().String(), int64(0), json.RawMessage("{}")); e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(map[string]any{"inboxId": inboxID.String()})
	job := jobs.Job{ID: uuid.New(), InstallationID: &installID, Payload: payload}
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	var outboxPayload []byte
	if e := f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_event_outbox`).Scan(&outboxPayload); e != nil {
		t.Fatal(e)
	}
	var envelope struct {
		SourceOccurredAt *time.Time `json:"sourceOccurredAt"`
	}
	if e := json.Unmarshal(outboxPayload, &envelope); e != nil {
		t.Fatal(e)
	}
	if envelope.SourceOccurredAt != nil {
		t.Fatalf("missing source time must stay null, fabricated %v", envelope.SourceOccurredAt)
	}
}

// The exact source epoch must be propagated unchanged.
func TestDPTriggerWorkerExactTime(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	installID, accountID := f.Assignment.Scope.InstallationID, f.Assignment.Scope.AccountID
	worker := &DPTriggerWorker{Store: f.Store, CRM: f.CRM}
	const exact = int64(1491300016)
	inboxID := uuid.New()
	dedup := sha256.Sum256([]byte("exact-time"))
	if _, e := f.Pool.Exec(ctx, `INSERT INTO distribution_dp_inbox(id,dedup_key,installation_id,account_id,lead_id,pipeline_id,status_id,event_type,direction,group_id,occurred_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		inboxID, dedup[:], installID, accountID, int64(10), int64(20), int64(30), 15, "went_to_trigger", uuid.New().String(), exact, json.RawMessage("{}")); e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(map[string]any{"inboxId": inboxID.String()})
	job := jobs.Job{ID: uuid.New(), InstallationID: &installID, Payload: payload}
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	var outboxPayload []byte
	if e := f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_event_outbox`).Scan(&outboxPayload); e != nil {
		t.Fatal(e)
	}
	var envelope struct {
		SourceOccurredAt *time.Time `json:"sourceOccurredAt"`
	}
	if e := json.Unmarshal(outboxPayload, &envelope); e != nil {
		t.Fatal(e)
	}
	if envelope.SourceOccurredAt == nil || envelope.SourceOccurredAt.Unix() != exact {
		t.Fatalf("want unix %d, got %v", exact, envelope.SourceOccurredAt)
	}
}

// application/x-www-form-urlencoded with nested bracket keys and string ids,
// not JSON. The receiver must accept it, reject duplicates/structural conflicts/
// malformed brackets/invalid integers as 400, and keep auth+dedupe semantics.
func TestDPReceiverAcceptsURLEncodedForm(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	installID, accountID := f.Assignment.Scope.InstallationID, f.Assignment.Scope.AccountID
	const key = "dp-form-secret"
	hash := sha256.Sum256([]byte(key))
	if _, e := f.Pool.Exec(ctx, `UPDATE installations SET webhook_key_hash=$1, webhook_status='active' WHERE id=$2`, hash[:], installID); e != nil {
		t.Fatal(e)
	}
	receiver := &DPReceiver{Pool: f.Pool, Jobs: f.Jobs, Installations: installations.NewStore(f.Pool), MaxBody: MaxBody}
	groupID := uuid.New().String()
	base := func() url.Values {
		return dpFormBody(key, accountID, groupID, 15, "went_to_trigger", 10, 30, 20, 1491300016)
	}
	inboxCount := func() int {
		var n int
		if e := f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_dp_inbox`).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}

	// Captured form: accepted (202) with parsed ids, group and a job.
	ok := httptest.NewRecorder()
	receiver.Receive(ok, dpFormRequest(base()))
	if ok.Code != http.StatusAccepted {
		t.Fatalf("urlencoded trigger must 202, got %d %s", ok.Code, ok.Body.String())
	}
	var state string
	var gotGroup uuid.UUID
	var stored []byte
	var jobID uuid.NullUUID
	if e := f.Pool.QueryRow(ctx, `SELECT state,group_id,payload,job_id FROM distribution_dp_inbox`).Scan(&state, &gotGroup, &stored, &jobID); e != nil {
		t.Fatal(e)
	}
	if state != "pending" || !jobID.Valid || gotGroup.String() != groupID {
		t.Fatalf("form inbox not parsed: state=%s job=%v group=%s", state, jobID, gotGroup)
	}
	if strings.Contains(string(stored), key) {
		t.Fatal("webhook key must never be stored in the trigger evidence")
	}

	// Replay dedupes (200) and never duplicates.
	replay := httptest.NewRecorder()
	receiver.Receive(replay, dpFormRequest(base()))
	if replay.Code != http.StatusOK {
		t.Fatalf("form replay must dedupe 200, got %d", replay.Code)
	}
	if n := inboxCount(); n != 1 {
		t.Fatalf("form replay must not duplicate, count=%d", n)
	}

	// Wrong key / wrong account stay 404 and create nothing.
	wrong := map[string]*http.Request{
		"wrong-key":     dpFormRequest(dpFormBody("wrong-key", accountID, groupID, 15, "went_to_trigger", 10, 30, 20, 1491300017)),
		"wrong-account": dpFormRequest(dpFormBody(key, accountID+1, groupID, 15, "went_to_trigger", 10, 30, 20, 1491300018)),
	}
	for name, r := range wrong {
		rec := httptest.NewRecorder()
		receiver.Receive(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s must 404, got %d", name, rec.Code)
		}
	}
	if n := inboxCount(); n != 1 {
		t.Fatalf("rejected auth must not create inbox, count=%d", n)
	}

	// Strict rejections: duplicates, structural conflict, malformed brackets and
	// invalid integers are 400 and never enqueue a job.
	dup := func(form url.Values) {
		form["account_id"] = []string{form.Get("account_id"), form.Get("account_id")}
	}
	dupGroup := func(form url.Values) {
		k := "action[settings][widget][settings][groupId]"
		form[k] = []string{form.Get(k), form.Get(k)}
	}
	dupKey := func(form url.Values) {
		k := "action[settings][widget][settings][key]"
		form[k] = []string{form.Get(k), form.Get(k)}
	}
	cases := map[string]func(url.Values){
		"duplicate-account":   dup,
		"duplicate-groupid":   dupGroup,
		"duplicate-key":       dupKey,
		"structural-conflict": func(form url.Values) { form.Set("event[data]", "scalar") },
		"malformed-bracket":   func(form url.Values) { form.Set("event[type", "15") },
		"invalid-integer":     func(form url.Values) { form.Set("event[data][id]", "not-a-number") },
	}
	for name, mut := range cases {
		form := base()
		form.Set("event[time]", "1491300100")
		mut(form)
		rec := httptest.NewRecorder()
		receiver.Receive(rec, dpFormRequest(form))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must 400, got %d", name, rec.Code)
		}
	}
	if n := inboxCount(); n != 1 {
		t.Fatalf("rejected forms must not create inbox, count=%d", n)
	}
	var dpJobs int
	if e := f.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type=$1`, DPTriggerJobType).Scan(&dpJobs); e != nil || dpJobs != 1 {
		t.Fatalf("only the accepted form may enqueue a job, jobs=%d err=%v", dpJobs, e)
	}

	// JSON compatibility: a fresh JSON trigger still 202s.
	jsBody := fmt.Sprintf(`{"event":{"type":15,"type_code":"lead_appeared_in_status","time":1491300111,"data":{"id":11,"element_type":2,"status_id":30,"pipeline_id":20,"direction_of_movement":"went_to_trigger"}},"action":{"settings":{"widget":{"settings":{"key":%q,"groupId":%q}}}},"subdomain":"test","account_id":%d}`, key, uuid.New().String(), accountID)
	js := httptest.NewRecorder()
	jsReq := httptest.NewRequest(http.MethodPost, "/api/v1/widget/distribution/dp", strings.NewReader(jsBody))
	jsReq.Header.Set("Content-Type", "application/json")
	receiver.Receive(js, jsReq)
	if js.Code != http.StatusAccepted {
		t.Fatalf("json trigger must 202, got %d %s", js.Code, js.Body.String())
	}
}

func TestDPReceiverDurableInboxDedupeRedactionAndWorker(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	installID, accountID := f.Assignment.Scope.InstallationID, f.Assignment.Scope.AccountID
	const key = "dp-scoped-secret"
	hash := sha256.Sum256([]byte(key))
	if _, e := f.Pool.Exec(ctx, `UPDATE installations SET webhook_key_hash=$1, webhook_status='active' WHERE id=$2`, hash[:], installID); e != nil {
		t.Fatal(e)
	}
	receiver := &DPReceiver{Pool: f.Pool, Jobs: f.Jobs, Installations: installations.NewStore(f.Pool), MaxBody: MaxBody}
	groupID := uuid.New().String()

	// Unsafe requests never create durable state.
	bad := httptest.NewRecorder()
	receiver.Receive(bad, dpRequest(t, "wrong-key", accountID, 15, "went_to_trigger", groupID))
	if bad.Code != http.StatusNotFound {
		t.Fatalf("bad key must 404, got %d", bad.Code)
	}
	mismatch := httptest.NewRecorder()
	receiver.Receive(mismatch, dpRequest(t, key, accountID+1, 15, "went_to_trigger", groupID))
	if mismatch.Code != http.StatusNotFound {
		t.Fatalf("account mismatch must 404, got %d", mismatch.Code)
	}
	malformed := httptest.NewRecorder()
	receiver.Receive(malformed, httptest.NewRequest(http.MethodPost, "/dp", strings.NewReader("{not json")))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed must 400, got %d", malformed.Code)
	}
	outgoing := httptest.NewRecorder()
	receiver.Receive(outgoing, dpRequest(t, key, accountID, 15, "went_from_trigger", groupID))
	if outgoing.Code != http.StatusAccepted {
		t.Fatalf("went_from_trigger must be ignored with 202, got %d", outgoing.Code)
	}
	var ignored int
	if e := f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_dp_inbox`).Scan(&ignored); e != nil || ignored != 0 {
		t.Fatalf("ignored requests must not persist, count=%d err=%v", ignored, e)
	}

	// Valid trigger: durable inbox + job, accepted fast, key redacted.
	ok := httptest.NewRecorder()
	receiver.Receive(ok, dpRequest(t, key, accountID, 15, "went_to_trigger", groupID))
	if ok.Code != http.StatusAccepted {
		t.Fatalf("valid trigger must 202, got %d %s", ok.Code, ok.Body.String())
	}
	var inboxID uuid.UUID
	var state string
	var stored []byte
	var jobID uuid.NullUUID
	if e := f.Pool.QueryRow(ctx, `SELECT id,state,payload,job_id FROM distribution_dp_inbox`).Scan(&inboxID, &state, &stored, &jobID); e != nil {
		t.Fatal(e)
	}
	if state != "pending" || !jobID.Valid {
		t.Fatalf("inbox must be pending with a job: state=%s job=%v", state, jobID)
	}
	if strings.Contains(string(stored), key) {
		t.Fatal("webhook key must never be stored in the trigger evidence")
	}

	// Replay: deduped, no second inbox row.
	replay := httptest.NewRecorder()
	receiver.Receive(replay, dpRequest(t, key, accountID, 15, "went_to_trigger", groupID))
	if replay.Code != http.StatusOK {
		t.Fatalf("replay must be idempotent 200, got %d", replay.Code)
	}
	var count int
	if e := f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_dp_inbox`).Scan(&count); e != nil || count != 1 {
		t.Fatalf("replay must not duplicate, count=%d err=%v", count, e)
	}

	// Worker: read lead, normalize, write outbox. CRM failure retries; success settles.
	worker := &DPTriggerWorker{Store: f.Store, CRM: f.CRM}
	f.CRM.ObserveErr = context.DeadlineExceeded
	payload, _ := json.Marshal(map[string]any{"inboxId": inboxID.String()})
	job := jobs.Job{ID: jobID.UUID, InstallationID: &installID, Payload: payload}
	if _, e := worker.Handler(ctx, job); e == nil {
		t.Fatal("CRM failure must surface for retry")
	}
	if e := f.Pool.QueryRow(ctx, `SELECT state FROM distribution_dp_inbox WHERE id=$1`, inboxID).Scan(&state); e != nil || state != "pending" {
		t.Fatalf("inbox must stay pending on CRM failure, state=%s err=%v", state, e)
	}
	f.CRM.ObserveErr = nil
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	if e := f.Pool.QueryRow(ctx, `SELECT state FROM distribution_dp_inbox WHERE id=$1`, inboxID).Scan(&state); e != nil || state != "processed" {
		t.Fatalf("inbox must be processed, state=%s err=%v", state, e)
	}
	var outboxPayload []byte
	var outboxKind string
	if e := f.Pool.QueryRow(ctx, `SELECT payload->'event'->>'kind', payload FROM distribution_event_outbox`).Scan(&outboxKind, &outboxPayload); e != nil {
		t.Fatal(e)
	}
	if outboxKind != "lead.digital_pipeline_trigger" || strings.Contains(string(outboxPayload), key) {
		t.Fatalf("outbox must carry the DP trigger without the key: %s", outboxKind)
	}
	var envelope struct {
		SourceOccurredAt *time.Time `json:"sourceOccurredAt"`
	}
	if e := json.Unmarshal(outboxPayload, &envelope); e != nil {
		t.Fatal(e)
	}
	if envelope.SourceOccurredAt == nil || envelope.SourceOccurredAt.Unix() != 1491300016 {
		t.Fatalf("DP envelope must carry the exact event time, got %v", envelope.SourceOccurredAt)
	}
	// Re-running the settled job is a no-op.
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	var outbox int
	if e := f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_event_outbox`).Scan(&outbox); e != nil || outbox != 1 {
		t.Fatalf("worker must be idempotent, outbox=%d err=%v", outbox, e)
	}
}
