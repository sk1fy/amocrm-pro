package distribution

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func seedEvent(t *testing.T, f *assignmentFixtureData) EventEnvelope {
	t.Helper()
	now := time.Now().UTC()
	env := EventEnvelope{SchemaVersion: 1, MessageID: uuid.New(), Scope: f.Assignment.Scope, EventID: uuid.New(), ReceivedAt: now, EmittedAt: now, CorrelationID: uuid.New(), Event: SourceEvent{Kind: "lead.snapshot_reconciled", LeadID: 10, ObservationRevision: 1}}
	raw, _ := json.Marshal(env)
	scope, _ := json.Marshal(env.Scope)
	if _, e := f.Pool.Exec(context.Background(), `INSERT INTO distribution_event_outbox(message_id,scope,payload) VALUES($1,$2,$3)`, env.MessageID, scope, raw); e != nil {
		t.Fatal(e)
	}
	return env
}
func TestDeliveryExactAckDuplicateRetryAndFence(t *testing.T) {
	f := assignmentFixture(t)
	env := seedEvent(t, f)
	ctx := context.Background()
	secret := "0123456789abcdef0123456789abcdef"
	mode := "wrong"
	var seen [][]byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body EventEnvelope
		raw, _ := io.ReadAll(r.Body)
		seen = append(seen, raw)
		if json.Unmarshal(raw, &body) != nil {
			t.Error("invalid body")
		}
		if r.Header.Get("X-Distribution-Key-Id") == "" {
			t.Error("signature missing")
		}
		receipt := DeliveryReceipt{env.MessageID, uuid.New(), "accepted", time.Now().UTC()}
		if mode == "wrong" {
			receipt.MessageID = uuid.New()
		}
		if mode == "duplicate" {
			receipt.Disposition = "duplicate"
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	worker := DeliveryWorker{Store: f.Store, URL: server.URL, KeyID: "service", Keys: map[string]string{"service": secret}, HTTP: server.Client()}
	m, e := worker.claim(ctx, "events")
	if e != nil {
		t.Fatal(e)
	}
	ack, code, blocked := worker.deliver(ctx, m)
	if ack != nil || code != "invalid_ack" || blocked {
		t.Fatal(ack, code, blocked)
	}
	if e = worker.settle(ctx, m, ack, code, blocked); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE distribution_event_outbox SET next_attempt_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	mode = "duplicate"
	m2, e := worker.claim(ctx, "events")
	if e != nil {
		t.Fatal(e)
	}
	if m.ID != m2.ID || string(m.Payload) != string(m2.Payload) {
		t.Fatal("retry changed identity/body")
	}
	ack, code, blocked = worker.deliver(ctx, m2)
	if ack == nil || code != "" || blocked {
		t.Fatal(ack, code, blocked)
	}
	if e = worker.settle(ctx, m2, ack, code, blocked); e != nil {
		t.Fatal(e)
	}
	var state string
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM distribution_event_outbox`).Scan(&state)
	if state != "acknowledged" {
		t.Fatal(state)
	}
	// A superseded token cannot acknowledge or reset the newer sender's row.
	if _, e = f.Pool.Exec(ctx, `UPDATE distribution_event_outbox SET state='delivering',lease_token=$1,lease_until=clock_timestamp()+interval '1 minute'`, uuid.New()); e != nil {
		t.Fatal(e)
	}
	_ = worker.settle(ctx, m2, ack, "", false)
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM distribution_event_outbox`).Scan(&state)
	if state != "delivering" {
		t.Fatal("stale settlement won", state)
	}
}
func TestHistoricalResultsDeliveredAfterRevocation(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Store.FinishJob(ctx, job, op.OperationID, "cancelled", "no_attempt", "", "cancelled_before_dispatch", nil, true, "no_request_sent"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE distribution_bindings SET state='revoked',revoked_at=clock_timestamp();UPDATE integration_services SET enabled=false`); e != nil {
		t.Fatal(e)
	}
	var received ResultEnvelope
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := json.NewDecoder(r.Body).Decode(&received); e != nil {
			t.Error(e)
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(DeliveryReceipt{received.MessageID, uuid.New(), "accepted", time.Now().UTC()})
	}))
	defer server.Close()
	worker := DeliveryWorker{Store: f.Store, URL: server.URL, KeyID: "service", Keys: map[string]string{"service": "0123456789abcdef0123456789abcdef"}, HTTP: server.Client()}
	if e = worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if received.Scope != f.Assignment.Scope || received.Result.LeadID != 10 {
		t.Fatal("historical scope lost", received)
	}
	var state string
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM distribution_result_outbox LIMIT 1`).Scan(&state)
	if state != "acknowledged" {
		t.Fatal(state)
	}
}

func TestDurableDeliveryDiagnosticsExposeUnfinishedAndBlockedWithoutPIILabels(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	if _, _, e := f.Store.Admit(ctx, f.Scope, uuid.NewString(), f.Assignment); e != nil {
		t.Fatal(e)
	}
	seedEvent(t, f)
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewDeliveryCollector(f.Pool))
	families, e := registry.Gather()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "amocrm_distribution_unfinished_operations" {
			for _, metric := range family.Metric {
				if metric.Gauge.GetValue() == 1 && metric.Label[0].GetValue() == "queued" {
					found = true
				}
			}
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "state", "kind", "consumer":
				default:
					t.Fatal("unbounded/PII label", label.GetName())
				}
			}
		}
	}
	if !found {
		t.Fatal("unfinished operations invisible")
	}
}

func TestFrozenLegacyResultPayloadRetainedAndDelivered(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	if _, _, e := f.Store.Admit(ctx, f.Scope, uuid.NewString(), f.Assignment); e != nil {
		t.Fatal(e)
	}
	// Emulate the frozen migration19 envelope: leadId did not exist in its wire
	// Operation. Stage05 transport must send those same bytes and message ID.
	if _, e := f.Pool.Exec(ctx, `UPDATE distribution_result_outbox SET payload=payload#-'{result,leadId}'`); e != nil {
		t.Fatal(e)
	}
	var original []byte
	if e := f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_result_outbox`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	validateRuntimeResponse(t, "ResultEnvelope", original)
	var received []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		var env ResultEnvelope
		if e := json.Unmarshal(received, &env); e != nil {
			t.Error(e)
		}
		if env.Result.LeadID != 0 {
			t.Error("legacy payload silently enriched")
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(DeliveryReceipt{env.MessageID, uuid.New(), "accepted", time.Now().UTC()})
	}))
	defer server.Close()
	worker := DeliveryWorker{Store: f.Store, URL: server.URL, KeyID: "service", Keys: map[string]string{"service": "0123456789abcdef0123456789abcdef"}, HTTP: server.Client()}
	if e := worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if string(received) != string(original) {
		t.Fatal("legacy frozen bytes changed")
	}
	var after []byte
	var state string
	_ = f.Pool.QueryRow(ctx, `SELECT payload,state FROM distribution_result_outbox`).Scan(&after, &state)
	if state != "acknowledged" || string(after) != string(original) {
		t.Fatal("legacy delivery lost immutable version", state)
	}
}
func TestSourceReadinessRequiresCompleteStage05Subscription(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	b, e := f.Store.Get(ctx, f.Scope, f.Assignment.Scope.BindingID)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		settings string
		ready    bool
	}{{`["add_lead","status_lead"]`, false}, {`["add_lead","update_lead","status_lead","responsible_lead","delete_lead"]`, true}} {
		if _, e = f.Pool.Exec(ctx, `UPDATE installations SET webhook_status='active',webhook_settings=$2 WHERE id=$1`, f.Scope.InstallationID, tc.settings); e != nil {
			t.Fatal(e)
		}
		ready, e := f.Store.Readiness(ctx, b)
		if e != nil || ready["subscriptionReady"] != tc.ready {
			t.Fatal("incomplete source readiness", ready, e)
		}
	}
}

func TestInvalidDurableEnvelopeBlocksWithoutStarvingValidMessage(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	bad := uuid.New()
	scope, _ := json.Marshal(f.Assignment.Scope)
	payload, _ := json.Marshal(map[string]any{"messageId": bad, "scope": map[string]any{}})
	if _, e := f.Pool.Exec(ctx, `INSERT INTO distribution_event_outbox(message_id,scope,payload) VALUES($1,$2,$3)`, bad, scope, payload); e != nil {
		t.Fatal(e)
	}
	valid := seedEvent(t, f)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env EventEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		if env.MessageID != valid.MessageID {
			t.Error("invalid envelope dispatched")
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(DeliveryReceipt{env.MessageID, uuid.New(), "accepted", time.Now().UTC()})
	}))
	defer server.Close()
	worker := DeliveryWorker{Store: f.Store, URL: server.URL, KeyID: "service", Keys: map[string]string{"service": "0123456789abcdef0123456789abcdef"}, HTTP: server.Client()}
	if e := worker.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	var state, code string
	_ = f.Pool.QueryRow(ctx, `SELECT state,error_code FROM distribution_event_outbox WHERE message_id=$1`, bad).Scan(&state, &code)
	if state != "blocked" || code != "invalid_envelope" {
		t.Fatal(state, code)
	}
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM distribution_event_outbox WHERE message_id=$1`, valid.MessageID).Scan(&state)
	if state != "acknowledged" {
		t.Fatal("bad row starved valid row", state)
	}
}
