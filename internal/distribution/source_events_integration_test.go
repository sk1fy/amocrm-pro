package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"testing"
	"time"
)

func freezeSource(t *testing.T, f *assignmentFixtureData, s *webhook.Store, raw string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if _, e := f.Pool.Exec(ctx, `UPDATE installations SET webhook_settings='["add_lead","update_lead","status_lead","responsible_lead","delete_lead"]' WHERE id=$1`, f.Scope.InstallationID); e != nil {
		t.Fatal(e)
	}
	id, e := s.SaveDeliveryAndEnqueue(ctx, f.Scope.InstallationID, uuid.New(), "application/x-www-form-urlencoded", []byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func parseSource(t *testing.T, f *assignmentFixtureData, s *webhook.Store, id uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	d, e := s.GetDelivery(ctx, id, f.Scope.InstallationID)
	if e != nil {
		t.Fatal(e)
	}
	events, e := webhook.ParseAllowed(d.InstallationID, d.RawBody, d.WebhookEvents)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveParsedEvents(ctx, d, events); e != nil {
		t.Fatal(e)
	}
	var event uuid.UUID
	if e = f.Pool.QueryRow(ctx, `SELECT id FROM inbox_events WHERE delivery_id=$1`, id).Scan(&event); e != nil {
		t.Fatal(e)
	}
	return event
}
func claimSource(t *testing.T, f *assignmentFixtureData) jobs.Job {
	t.Helper()
	claimed, e := f.Jobs.Claim(context.Background(), "source-worker", 100, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	for _, job := range claimed {
		if job.Type == NormalizeEventJobType {
			return job
		}
	}
	t.Fatal("source job missing")
	return jobs.Job{}
}
func TestSourceIngressSnapshotIsolationAndFrozenOutbox(t *testing.T) {
	f := assignmentFixture(t)
	if _, e := f.Pool.Exec(context.Background(), `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-status',true)`, f.Assignment.Scope.IntegrationID); e != nil {
		t.Fatal(e)
	}
	s := composedSourceStore(f)
	ctx := context.Background()
	if _, e := f.Pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1`, f.Assignment.Scope.IntegrationID); e != nil {
		t.Fatal(e)
	}
	old := freezeSource(t, f, s, "leads[update][0][id]=10&leads[update][0][last_modified]=100")
	if _, e := f.Pool.Exec(ctx, `UPDATE integration_services SET enabled=true WHERE integration_id=$1`, f.Assignment.Scope.IntegrationID); e != nil {
		t.Fatal(e)
	}
	parseSource(t, f, s, old)
	var n int
	_ = f.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_consumer_receipts WHERE consumer_id=$1`, webhook.DistributionConsumer).Scan(&n)
	if n != 0 {
		t.Fatal("retroactive consumer", n)
	}
	current := freezeSource(t, f, s, "leads[status][0][id]=10&leads[status][0][status_id]=30&leads[status][0][old_status_id]=29&leads[status][0][pipeline_id]=20&leads[status][0][last_modified]=100")
	event := parseSource(t, f, s, current)
	s.RegisterEventRouter("leads", "status", func(context.Context, pgx.Tx, services.Event) (services.EventRoute, error) {
		return services.EventRoute{}, errors.New("legacy failure")
	})
	if e := s.ProcessEvent(ctx, event, f.Scope.InstallationID); e == nil {
		t.Fatal("expected independent legacy failure")
	}
	job := claimSource(t, f)
	worker := SourceWorker{f.Store, s, f.CRM}
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	var first []byte
	_ = f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_event_outbox`).Scan(&first)
	validateRuntimeResponse(t, "EventEnvelope", first)
	var env EventEnvelope
	if e := json.Unmarshal(first, &env); e != nil {
		t.Fatal(e)
	}
	if env.Event.EntryFingerprint != nil || env.Event.Before != nil || env.Event.Kind != "lead.status_changed" || env.Event.SourceEvidence.OldStatusID == nil {
		t.Fatalf("fabricated historical proof %+v", env.Event)
	}
	if _, e := worker.Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	var again []byte
	_ = f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_event_outbox`).Scan(&again)
	if string(first) != string(again) {
		t.Fatal("frozen envelope changed on retry")
	}
}
func TestSourceFenceAndCleanupReplay(t *testing.T) {
	f := assignmentFixture(t)
	s := composedSourceStore(f)
	ctx := context.Background()
	raw := "leads[update][0][id]=10&leads[update][0][last_modified]=100"
	id := freezeSource(t, f, s, raw)
	parseSource(t, f, s, id)
	job := claimSource(t, f)
	if _, e := f.Pool.Exec(ctx, `UPDATE jobs SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := (&SourceWorker{f.Store, s, f.CRM}).Handler(ctx, job); !errors.Is(e, jobs.ErrLeaseLost) {
		t.Fatal("stale normalizer published", e)
	}
	if _, e := f.Pool.Exec(ctx, `DELETE FROM inbox_events; DELETE FROM webhook_deliveries; DELETE FROM webhook_event_tombstones`); e != nil {
		t.Fatal(e)
	}
	replay := freezeSource(t, f, s, raw)
	d, e := s.GetDelivery(ctx, replay, f.Scope.InstallationID)
	if e != nil {
		t.Fatal(e)
	}
	events, e := webhook.ParseAllowed(d.InstallationID, d.RawBody, d.WebhookEvents)
	if e != nil {
		t.Fatal(e)
	}
	n, e := s.SaveParsedEvents(ctx, d, events)
	if e != nil || n != 0 {
		t.Fatal("retained receipt lost", n, e)
	}
}

type observationFake struct {
	Lead           amocrm.LeadState
	Err            error
	Enter, Release chan struct{}
}

func (c *observationFake) GetLeadSnapshot(context.Context, uuid.UUID, int64) (amocrm.LeadState, error) {
	if c.Enter != nil {
		close(c.Enter)
		<-c.Release
	}
	return c.Lead, c.Err
}
func TestObservationOrderingAndDocumentedAbsence(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	old := &observationFake{Lead: f.CRM.Lead, Enter: make(chan struct{}), Release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, e := f.Store.ObserveLead(ctx, old, f.Assignment.Scope, 10); done <- e }()
	<-old.Enter
	newer := &observationFake{Lead: f.CRM.Lead}
	newer.Lead.StatusID = 40
	o, e := f.Store.ObserveLead(ctx, newer, f.Assignment.Scope, 10)
	if e != nil {
		t.Fatal(e)
	}
	close(old.Release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	var raw []byte
	var revision int64
	if e = f.Pool.QueryRow(ctx, `SELECT snapshot,observation_revision FROM distribution_source_heads WHERE account_id=123 AND lead_id=10`).Scan(&raw, &revision); e != nil {
		t.Fatal(e)
	}
	var head Snapshot
	_ = json.Unmarshal(raw, &head)
	if head.StatusID != 40 || revision != o.ObservationRevision {
		t.Fatal("old GET overwrote new head", head, revision)
	}
	absent, e := f.Store.ObserveLead(ctx, &observationFake{Err: amocrm.ErrLeadAbsent}, f.Assignment.Scope, 10)
	if e != nil || !absent.Absent || absent.Deleted || absent.Snapshot != nil {
		t.Fatal(absent, e)
	}
	body, _ := json.Marshal(absent)
	validateRuntimeResponse(t, "LeadObservation", body)
	for _, status := range []int{403, 404} {
		if _, e = f.Store.ObserveLead(ctx, &observationFake{Err: &amocrm.APIError{StatusCode: status}}, f.Assignment.Scope, 10); e == nil {
			t.Fatal("undocumented denial inferred absence", status)
		}
	}
}

func composedSourceStore(f *assignmentFixtureData) *webhook.Store {
	s := webhook.NewStore(f.Pool)
	w := SourceWorker{f.Store, s, f.CRM}
	w.RegisterEvents(s, map[string]jobs.Handler{})
	return s
}

func TestComposedConsumersPreserveLegacyCorrelationAndResponsibleNonEntry(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	if _, e := f.Pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-status',true)`, f.Assignment.Scope.IntegrationID); e != nil {
		t.Fatal(e)
	}
	s := webhook.NewStore(f.Pool)
	handlers := map[string]jobs.Handler{"webhook.process_event": webhook.ProcessEventJobHandler(s)}
	w := SourceWorker{f.Store, s, f.CRM}
	w.RegisterEvents(s, handlers)
	effect, correlationJob := uuid.New(), uuid.New()
	if _, e := f.Pool.Exec(ctx, `INSERT INTO jobs(id,installation_id,type) VALUES($1,$2,'workflow.lead.set_status')`, correlationJob, f.Scope.InstallationID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Pool.Exec(ctx, `INSERT INTO outbound_effects(id,installation_id,correlation_job_id,effect_type,resource_type,resource_id,desired_state,desired_hash,correlation_expires_at) VALUES($1,$2,$3,'lead.set_status','lead','10','{}',decode(repeat('00',32),'hex'),clock_timestamp()+interval '1 minute')`, effect, f.Scope.InstallationID, correlationJob); e != nil {
		t.Fatal(e)
	}
	s.RegisterEventRouter("leads", "status", func(context.Context, pgx.Tx, services.Event) (services.EventRoute, error) {
		return services.EventRoute{Workflow: "lead-status", Disposition: "correlated_effect", EffectID: &effect}, nil
	})
	id := freezeSource(t, f, s, "leads[status][0][id]=10&leads[status][0][old_status_id]=29&leads[status][0][status_id]=30&leads[status][0][last_modified]=100")
	event := parseSource(t, f, s, id)
	payload, _ := json.Marshal(map[string]any{"event_id": event})
	legacyJob := jobs.Job{Type: "webhook.process_event", InstallationID: &f.Scope.InstallationID, Payload: payload}
	if _, e := handlers[legacyJob.Type](ctx, legacyJob); e != nil {
		t.Fatal(e)
	}
	job := claimSource(t, f)
	if _, e := handlers[job.Type](ctx, job); e != nil {
		t.Fatal(e)
	}
	var legacy, distribution string
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM webhook_consumer_receipts WHERE event_id=$1 AND consumer_id=$2`, event, webhook.LeadStatusConsumer).Scan(&legacy)
	_ = f.Pool.QueryRow(ctx, `SELECT state FROM webhook_consumer_receipts WHERE event_id=$1 AND consumer_id=$2`, event, webhook.DistributionConsumer).Scan(&distribution)
	if legacy != "ignored" || distribution != "processed" {
		t.Fatal("global suppression", legacy, distribution)
	}
	f.CRM.Lead.ResponsibleUserID = 2
	id = freezeSource(t, f, s, "leads[responsible][0][id]=10&leads[responsible][0][responsible_user_id]=2&leads[responsible][0][last_modified]=101")
	parseSource(t, f, s, id)
	job = claimSource(t, f)
	if _, e := handlers[job.Type](ctx, job); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	_ = f.Pool.QueryRow(ctx, `SELECT payload FROM distribution_event_outbox ORDER BY created_at DESC LIMIT 1`).Scan(&raw)
	var env EventEnvelope
	_ = json.Unmarshal(raw, &env)
	if env.Event.Kind != "lead.responsible_changed" || env.Event.EntryFingerprint != nil || env.Event.Before != nil {
		t.Fatal("own responsible change became entry", env)
	}
	var operations int
	_ = f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_operations`).Scan(&operations)
	if operations != 0 {
		t.Fatal("source recursively assigns", operations)
	}
}
