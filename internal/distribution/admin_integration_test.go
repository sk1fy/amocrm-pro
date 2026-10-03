package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"os"
	"strings"
	"testing"
)

func TestAdminPausePreservesSourceAndUnknownEvidence(t *testing.T) {
	f := assignmentFixture(t)
	job := f.AdmitAndClaim(t)
	f.CRM.Mode = "timeout_applied"
	if _, e := f.Worker().Handler(t.Context(), job); e != nil {
		t.Fatal(e)
	}
	op, e := f.Store.Operation(t.Context(), f.Scope, f.Assignment.Command.OperationID)
	if e != nil || op.State != "outcome_unknown" {
		t.Fatal(op, e)
	}
	ctx := context.Background()
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	expected := false
	if _, e = AdminApplyTx(ctx, tx, f.Scope.InstallationID, "distribution-pause", AdminCommand{ExpectedPaused: &expected}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = f.Store.Require(ctx, f.Assignment.Scope.Binding()); e != nil {
		t.Fatal("pause disabled source/read authority", e)
	}
	event := seedEvent(t, f)
	if event.MessageID == uuid.Nil {
		t.Fatal("source envelope missing")
	}
	another := f.Assignment
	another.Command.OperationID = uuid.New()
	another.Command.DecisionID = uuid.New()
	another.Command.Expected.LeadID = 11
	if _, _, e = f.Store.Admit(ctx, f.Scope, uuid.NewString(), another); !errors.Is(e, ErrDenied) {
		t.Fatal("new admission not stopped", e)
	}
	receipt := uuid.New()
	verified, e := f.Worker().AdminReconcile(ctx, f.Scope.InstallationID, op.OperationID, receipt, op.ResultVersion, "employee:fixture")
	if e != nil || verified.State != "outcome_unknown" || verified.ExternalEffectState != "unknown" || f.CRM.Calls != 1 {
		t.Fatal("unknown became success or second PATCH", verified, e, f.CRM.Calls)
	}
	var guards int
	if e = f.Pool.QueryRow(ctx, `SELECT count(*) FROM distribution_lead_guards WHERE operation_id=$1`, op.OperationID).Scan(&guards); e != nil || guards != 1 {
		t.Fatal("unknown guard lost", guards, e)
	}
	replay, e := f.Worker().AdminReconcile(ctx, f.Scope.InstallationID, op.OperationID, receipt, op.ResultVersion, "employee:fixture")
	if e != nil || replay.ResultVersion != verified.ResultVersion {
		t.Fatal("replay changed version", replay, e)
	}
	tx, e = f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	expected = true
	if _, e = AdminApplyTx(ctx, tx, f.Scope.InstallationID, "distribution-resume", AdminCommand{ExpectedPaused: &expected}); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if _, _, e = f.Store.Admit(ctx, f.Scope, uuid.NewString(), another); e != nil {
		t.Fatal("resume failed", e)
	}
}
func TestAdminPauseAfterObservationStopsDispatch(t *testing.T) {
	f := assignmentFixture(t)
	job := f.AdmitAndClaim(t)
	f.CRM.PrepareHook = func() {
		if _, e := f.Pool.Exec(t.Context(), `UPDATE distribution_admin_pauses SET paused=true WHERE installation_id=$1`, f.Scope.InstallationID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.Worker().Handler(t.Context(), job); !errors.Is(e, ErrDenied) {
		t.Fatal("pause failed to stop dispatch", e)
	}
	if f.CRM.Calls != 0 {
		t.Fatal("PATCH despite pause")
	}
	var attempts int
	if e := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM distribution_operation_attempts`).Scan(&attempts); e != nil || attempts != 0 {
		t.Fatal(attempts, e)
	}
}
func TestAdminDeliveryRetryPreservesBodyAndChecksScope(t *testing.T) {
	f := assignmentFixture(t)
	event := seedEvent(t, f)
	if _, e := f.Pool.Exec(t.Context(), `UPDATE distribution_event_outbox SET state='blocked',attempts=20,error_code='invalid_ack'`); e != nil {
		t.Fatal(e)
	}
	var before string
	_ = f.Pool.QueryRow(t.Context(), `SELECT payload::text FROM distribution_event_outbox`).Scan(&before)
	for _, tc := range []struct {
		install  uuid.UUID
		attempts int
		ok       bool
	}{{uuid.New(), 20, false}, {f.Scope.InstallationID, 19, false}, {f.Scope.InstallationID, 20, true}} {
		tx, e := f.Pool.Begin(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		_, e = AdminApplyTx(t.Context(), tx, tc.install, "distribution-delivery-retry", AdminCommand{Kind: "events", MessageID: event.MessageID.String(), ExpectedAttempts: &tc.attempts})
		if tc.ok {
			if e != nil {
				t.Fatal(e)
			}
			if e = tx.Commit(t.Context()); e != nil {
				t.Fatal(e)
			}
		} else {
			if e == nil {
				t.Fatal("bad precondition accepted")
			}
			_ = tx.Rollback(t.Context())
		}
	}
	var after, state string
	var attempts int
	if e := f.Pool.QueryRow(t.Context(), `SELECT payload::text,state,attempts FROM distribution_event_outbox`).Scan(&after, &state, &attempts); e != nil || before != after || state != "pending" || attempts != 0 {
		t.Fatal("changed frozen body", state, attempts, e)
	}
}

func TestAdminMetricsHaveBoundedLabelsAndNoIDs(t *testing.T) {
	f := assignmentFixture(t)
	job := f.AdmitAndClaim(t)
	f.CRM.Mode = "timeout_applied"
	if _, e := f.Worker().Handler(t.Context(), job); e != nil {
		t.Fatal(e)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewDeliveryCollector(f.Pool))
	metrics, e := registry.Gather()
	if e != nil {
		t.Fatal(e)
	}
	errorFound := false
	for _, family := range metrics {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "kind", "state", "consumer", "class":
				default:
					t.Fatal("unbounded metric label", label.GetName())
				}
				if strings.Contains(label.GetValue(), f.Scope.InstallationID.String()) {
					t.Fatal("installation ID leaked")
				}
			}
		}
		if family.GetName() == "amocrm_distribution_operation_errors" {
			errorFound = true
		}
	}
	if !errorFound {
		t.Fatal("missing bounded error observations")
	}
}

type pauseSourceCRM struct{ fixture *assignmentFixtureData }

func (c pauseSourceCRM) GetLeadSnapshot(ctx context.Context, installation uuid.UUID, lead int64) (amocrm.LeadState, error) {
	if _, e := c.fixture.Pool.Exec(ctx, `UPDATE distribution_admin_pauses SET paused=true WHERE installation_id=$1`, installation); e != nil {
		return amocrm.LeadState{}, e
	}
	return c.fixture.CRM.GetLeadSnapshot(ctx, installation, lead)
}
func TestAdminPauseDuringSourceObservationPreservesAcceptedEvent(t *testing.T) {
	f := assignmentFixture(t)
	source := composedSourceStore(f)
	delivery := freezeSource(t, f, source, "leads[update][0][id]=10&leads[update][0][last_modified]=100")
	parseSource(t, f, source, delivery)
	job := claimSource(t, f)
	worker := SourceWorker{f.Store, source, pauseSourceCRM{f}}
	if _, e := worker.Handler(t.Context(), job); e != nil {
		t.Fatal(e)
	}
	var state string
	var n int
	if e := f.Pool.QueryRow(t.Context(), `SELECT state FROM webhook_consumer_receipts WHERE job_id=$1`, job.ID).Scan(&state); e != nil || state != "processed" {
		t.Fatal("accepted source lost during pause", state, e)
	}
	if _, e := f.Pool.Exec(t.Context(), `UPDATE distribution_admin_pauses SET paused=false WHERE installation_id=$1`, f.Scope.InstallationID); e != nil {
		t.Fatal(e)
	}
	if _, e := worker.Handler(t.Context(), job); e != nil {
		t.Fatal(e)
	}
	if e := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM distribution_event_outbox`).Scan(&n); e != nil || n != 1 {
		t.Fatal("source replay duplicated envelope", n, e)
	}
}
func TestAdminPauseMigrationDownRefusesActivePause(t *testing.T) {
	f := assignmentFixture(t)
	if _, e := f.Pool.Exec(t.Context(), `UPDATE distribution_admin_pauses SET paused=true WHERE installation_id=$1`, f.Scope.InstallationID); e != nil {
		t.Fatal(e)
	}
	migration, e := os.ReadFile("../../migrations/000021_distribution_admin.down.sql")
	if e != nil {
		t.Fatal(e)
	}
	tx, e := f.Pool.Begin(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(t.Context(), string(migration)); e == nil {
		t.Fatal("rollback erased an active operational pause")
	}
	_ = tx.Rollback(t.Context())
	if e = requireAdmission(t.Context(), f.Pool, f.Scope.InstallationID, false); !errors.Is(e, ErrDenied) {
		t.Fatal("rollback changed pause", e)
	}
}
