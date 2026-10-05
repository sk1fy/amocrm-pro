package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/maintenance"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAssignmentAtomicConcurrentReplayAndSemanticConflict(t *testing.T) {
	f := assignmentFixture(t)
	key := uuid.NewString()
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _, e := f.Store.Admit(ctx, f.Scope, key, f.Assignment)
			if e == nil && r.OperationID != f.Assignment.Command.OperationID {
				e = errors.New("changed operation")
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var operations, jobsCount, guards, results, outbox int
	if e := f.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM distribution_operations),(SELECT count(*) FROM jobs),(SELECT count(*) FROM distribution_lead_guards),(SELECT count(*) FROM distribution_operation_results),(SELECT count(*) FROM distribution_result_outbox)`).Scan(&operations, &jobsCount, &guards, &results, &outbox); e != nil || operations != 1 || jobsCount != 1 || guards != 1 || results != 1 || outbox != 1 {
		t.Fatal(operations, jobsCount, guards, results, outbox, e)
	}
	changed := f.Assignment
	changed.Command.TargetResponsibleUserID = 3
	if _, _, e := f.Store.Admit(ctx, f.Scope, key, changed); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatal("payload conflict", e)
	}
	// UTC normalization treats alternative offsets of the same timestamps equally.
	same := f.Assignment
	same.SourceOccurredAt = same.SourceOccurredAt.In(time.FixedZone("other", 10800))
	if _, replayed, e := f.Store.Admit(ctx, f.Scope, key, same); e != nil || !replayed {
		t.Fatal("semantic timestamp", e)
	}
	if _, e := f.Pool.Exec(ctx, `UPDATE distribution_bindings SET state='revoked' WHERE id=$1`, f.Assignment.Scope.BindingID); e != nil {
		t.Fatal(e)
	}
	// Replays don't need current admission authority or unexpired authorization.
	if _, replayed, e := f.Store.Admit(ctx, f.Scope, key, f.Assignment); e != nil || !replayed {
		t.Fatal("revoked receipt replay", e)
	}
	wrong := f.Scope
	wrong.CompanyID = uuid.New()
	if _, e := f.Store.Operation(ctx, wrong, f.Assignment.Command.OperationID); !errors.Is(e, ErrNotFound) {
		t.Fatal("foreignread", e)
	}
}
func TestAssignmentWorkerSuccessConflictUnknownAndNotSent(t *testing.T) {
	for _, tc := range []struct {
		mode, state string
		released    bool
	}{{"", "succeeded", true}, {"manual_after_ack", "conflict", true}, {"timeout_applied", "outcome_unknown", false}, {"timeout_unapplied", "outcome_unknown", false}, {"before_send", "rejected", true}, {"stale_observation", "confirming", false}, {"observe_failure", "confirming", false}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := assignmentFixture(t)
			f.CRM.Mode = tc.mode
			job := f.AdmitAndClaim(t)
			if _, e := f.Worker().Handler(context.Background(), job); e != nil {
				t.Fatal(e)
			}
			op, e := f.Store.Operation(context.Background(), f.Scope, f.Assignment.Command.OperationID)
			if e != nil || op.State != tc.state || op.ResolutionEvidence.GuardReleasable != tc.released {
				t.Fatal(op.State, op.ResolutionEvidence, e)
			}
			var n int
			if e = f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM distribution_lead_guards`).Scan(&n); e != nil || (n == 0) != tc.released {
				t.Fatal("guard", n, e)
			}
			if f.CRM.Calls != 1 {
				t.Fatal("request count", f.CRM.Calls)
			}
			if !tc.released {
				f.CRM.ObserveErr = nil
				f.CRM.Lead.UpdatedAt = 200
				jobStore := f.Jobs
				if _, e = f.Pool.Exec(context.Background(), `UPDATE jobs SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); e != nil {
					t.Fatal(e)
				}
				reclaimed, e := jobStore.Claim(context.Background(), "new-worker", 1, time.Minute)
				if e != nil || len(reclaimed) != 1 {
					t.Fatal(reclaimed, e)
				}
				if _, e = f.Worker().Handler(context.Background(), reclaimed[0]); e != nil {
					t.Fatal(e)
				}
				if f.CRM.Calls != 1 {
					t.Fatal("restart replayed PATCH", f.CRM.Calls)
				}
			}
		})
	}
}
func TestAssignmentPreconditionsAndLeaseFence(t *testing.T) {
	for _, kind := range []string{"source_changed", "disabled", "recipient", "denied_policy", "expired_policy", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := assignmentFixture(t)
			job := f.AdmitAndClaim(t)
			ctx := context.Background()
			switch kind {
			case "source_changed":
				f.CRM.Lead.ResponsibleUserID = 3
			case "disabled":
				if _, e := f.Pool.Exec(ctx, `UPDATE integration_services SET enabled=false WHERE integration_id=$1`, f.Assignment.Scope.IntegrationID); e != nil {
					t.Fatal(e)
				}
			case "recipient":
				u := f.CRM.users[2]
				u.Rights.IsActive = ptr(false)
				f.CRM.users[2] = u
			case "denied_policy":
				f.Policy.Deny = true
			case "expired_policy":
				f.Policy.Expired = true
			case "cancel":
				if _, e := f.Store.Cancel(ctx, f.Scope, f.Assignment.Command.OperationID, 1); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := f.Worker().Handler(ctx, job); e != nil {
				t.Fatal(e)
			}
			if f.CRM.Calls != 0 {
				t.Fatal("unexpected PATCH")
			}
			if kind == "recipient" {
				op, err := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
				if err != nil || op.Error == nil || op.Error.Code != "recipient_unavailable" || !op.ResolutionEvidence.GuardReleasable {
					t.Fatal("recipient rejection lost recalculation evidence", op, err)
				}
			}
		})
	}
	f := assignmentFixture(t)
	job := f.AdmitAndClaim(t)
	if _, e := f.Pool.Exec(context.Background(), `UPDATE jobs SET attempts=attempts+1,locked_by='new-worker' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Worker().Handler(context.Background(), job); !errors.Is(e, jobs.ErrLeaseLost) {
		t.Fatal("stale executor", e)
	}
	if f.CRM.Calls != 0 {
		t.Fatal("stale PATCH")
	}
}
func TestAssignmentObservationCannotResolveBeforeLateResponse(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Store.Dispatch(ctx, job, op, time.Now().Add(3*time.Second)); e != nil {
		t.Fatal(e)
	}
	old, e := f.Worker().Observe(ctx, op)
	if e != nil {
		t.Fatal(e)
	}
	old.ResponsibleUserID = 2
	updated := time.Unix(101, 0).UTC()
	old.SourceUpdatedAt = &updated
	if e = f.Store.RecordResponse(ctx, op.OperationID, job.Attempts, true, false, 200, "", 101); e != nil {
		t.Fatal(e)
	}
	op, e = f.Store.Operation(ctx, f.Scope, op.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	result, e := f.Store.ReconcileObservation(ctx, f.Scope, op.OperationID, op.ResultVersion, &old)
	if e != nil || result.FinishedAt != nil || result.ResolutionEvidence.GuardReleasable {
		t.Fatal("pre-response observation released", result.State, e)
	}
	f.CRM.Lead.ResponsibleUserID = 2
	f.CRM.Lead.UpdatedAt = 101
	fresh, e := f.Worker().Observe(ctx, result)
	if e != nil {
		t.Fatal(e)
	}
	result, e = f.Store.ReconcileObservation(ctx, f.Scope, result.OperationID, result.ResultVersion, &fresh)
	if e != nil || result.State != "succeeded" {
		t.Fatal(result.State, e)
	}
}
func TestAssignmentGlobalGuardAcrossRebinding(t *testing.T) {
	f := assignmentFixture(t)
	job := f.AdmitAndClaim(t)
	f.CRM.Mode = "timeout_applied"
	if _, e := f.Worker().Handler(context.Background(), job); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e := f.Store.Revoke(ctx, f.Scope, f.Assignment.Scope.BindingID); e != nil {
		t.Fatal(e)
	}
	newID := uuid.New()
	if _, e := f.Pool.Exec(ctx, `INSERT INTO distribution_bindings(id,company_id,installation_id,integration_id,account_id,revision,intent_id,confirmed_by) VALUES($1,$2,$3,$4,123,1,$5,1)`, newID, f.Scope.CompanyID, f.Scope.InstallationID, f.Assignment.Scope.IntegrationID, uuid.New()); e != nil {
		t.Fatal(e)
	}
	next := f.Assignment
	next.Scope.BindingID = newID
	next.Command.OperationID = uuid.New()
	next.Command.DecisionID = uuid.New()
	if _, _, e := f.Store.Admit(ctx, f.Scope, uuid.NewString(), next); !errors.Is(e, ErrOperationUnresolved) {
		t.Fatal("rebind bypassed guard", e)
	}
}
func TestAssignmentDispatchRechecksLeaseAfterLockWait(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE jobs SET locked_until=clock_timestamp()+interval '150 milliseconds' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM distribution_operations WHERE id=$1 FOR UPDATE`, op.OperationID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- f.Store.Dispatch(ctx, job, op, time.Now().Add(3*time.Second)) }()
	time.Sleep(250 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; !errors.Is(e, jobs.ErrLeaseLost) {
		t.Fatal("expired waited lease", e)
	}
	a, e := f.Store.Attempt(ctx, op.OperationID)
	if e != nil || a.Exists {
		t.Fatal("expired executor saved dispatch", a, e)
	}
}
func TestAssignmentNoChangeAndDurableAcknowledgmentRecovery(t *testing.T) {
	for _, kind := range []string{"keep", "assign"} {
		t.Run(kind, func(t *testing.T) {
			f := assignmentFixture(t)
			f.Assignment.Command.DecisionKind = kind
			f.Assignment.Command.TargetResponsibleUserID = 1
			if _, e := f.Pool.Exec(context.Background(), `INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,1)`, f.Assignment.Scope.BindingID, uuid.New()); e != nil {
				t.Fatal(e)
			}
			job := f.AdmitAndClaim(t)
			if _, e := f.Worker().Handler(context.Background(), job); e != nil {
				t.Fatal(e)
			}
			op, e := f.Store.Operation(context.Background(), f.Scope, f.Assignment.Command.OperationID)
			want := "already_target"
			if kind == "keep" {
				want = "kept"
			}
			if e != nil || op.State != "no_change" || op.Outcome == nil || *op.Outcome != want || f.CRM.Calls != 0 {
				t.Fatal(op.State, op.Outcome, f.CRM.Calls, e)
			}
		})
	}
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Store.Dispatch(ctx, job, op, time.Now().Add(3*time.Second)); e != nil {
		t.Fatal(e)
	}
	// Crash after durable response evidence but before operation result commit.
	f.CRM.Lead.ResponsibleUserID = 2
	f.CRM.Lead.UpdatedAt = 101
	if e = f.Store.RecordResponse(ctx, op.OperationID, job.Attempts, true, false, 200, "", 101); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Pool.Exec(ctx, `UPDATE jobs SET locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	reclaimed, e := f.Jobs.Claim(ctx, "new-worker", 1, time.Minute)
	if e != nil || len(reclaimed) != 1 {
		t.Fatal(reclaimed, e)
	}
	if _, e = f.Worker().Handler(ctx, reclaimed[0]); e != nil {
		t.Fatal(e)
	}
	result, e := f.Store.Operation(ctx, f.Scope, op.OperationID)
	if e != nil || result.State != "succeeded" || f.CRM.Calls != 0 {
		t.Fatal("durable ACK recovery", result.State, f.CRM.Calls, e)
	}
}
func TestAssignmentExhaustedJobObserverPreservesOnlyPossibleEffects(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmtLead(int64(boolInt(attempted))), func(t *testing.T) {
			f := assignmentFixture(t)
			ctx := context.Background()
			handlers := map[string]jobs.Handler{}
			observers := map[string]jobs.FailureObserver{}
			f.Worker().RegisterJobs(handlers, observers)
			if handlers[AssignmentJobType] == nil || observers[AssignmentJobType] == nil {
				t.Fatal("assignment execution/recovery registration missing")
			}
			job := f.AdmitAndClaim(t)
			if attempted {
				op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
				if e != nil {
					t.Fatal(e)
				}
				if e = f.Store.Dispatch(ctx, job, op, time.Now().Add(3*time.Second)); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := f.Pool.Exec(ctx, `UPDATE jobs SET max_attempts=1,locked_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); e != nil {
				t.Fatal(e)
			}
			_, e := f.Jobs.ClaimWithObserver(ctx, "new-worker", 1, 100, time.Minute, observers[AssignmentJobType])
			if e != nil {
				t.Fatal(e)
			}
			op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
			if e != nil {
				t.Fatal(e)
			}
			if attempted {
				if op.State != "outcome_unknown" || op.FinishedAt != nil {
					t.Fatal("deadjobreleasedpossibleeffect", op.State)
				}
			} else if op.State != "rejected" || op.FinishedAt == nil {
				t.Fatal("deadjobdidnotsafelyfinish", op.State)
			}
			var guarded bool
			if e = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM distribution_lead_guards WHERE operation_id=$1)`, op.OperationID).Scan(&guarded); e != nil || guarded != attempted {
				t.Fatal("registered observer guard disposition", guarded, attempted, e)
			}
			var versions, outbox int
			if e = f.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM distribution_operation_results),(SELECT count(*) FROM distribution_result_outbox)`).Scan(&versions, &outbox); e != nil || versions != outbox || versions < 2 {
				t.Fatal("observermissingresultoutbox", versions, outbox, e)
			}
		})
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestAssignmentCleanerPinsOldUnknownIdentity(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	f.CRM.Mode = "timeout_applied"
	if _, e := f.Worker().Handler(ctx, job); e != nil {
		t.Fatal(e)
	}
	if _, e := f.Pool.Exec(ctx, `UPDATE jobs SET status='completed',locked_by=NULL,locked_until=NULL,created_at=now()-interval '10 days',updated_at=now()-interval '9 days',finished_at=now()-interval '9 days' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := maintenance.NewStore(f.Pool).Cleanup(ctx, maintenance.Policy{SafetyMargin: time.Minute, WebhookInboxRetention: time.Hour, WebhookDeliveryRetention: time.Hour, RedeliveryHorizon: 7 * 24 * time.Hour, TombstoneRetention: 90 * 24 * time.Hour, BatchSize: 100, MaxBatches: 2}); e != nil {
		t.Fatal(e)
	}
	var operations, jobsCount, guards int
	if e := f.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM distribution_operations),(SELECT count(*) FROM jobs),(SELECT count(*) FROM distribution_lead_guards)`).Scan(&operations, &jobsCount, &guards); e != nil || operations != 1 || jobsCount != 1 || guards != 1 {
		t.Fatal("cleanupforgotunresolvedeffect", operations, jobsCount, guards, e)
	}
}
func TestAssignmentFinalizationRechecksWaitedLease(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	job := f.AdmitAndClaim(t)
	if _, e := f.Pool.Exec(ctx, `UPDATE jobs SET locked_until=clock_timestamp()+interval '150 milliseconds' WHERE id=$1`, job.ID); e != nil {
		t.Fatal(e)
	}
	tx, e := f.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM distribution_operations WHERE id=$1 FOR UPDATE`, f.Assignment.Command.OperationID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.Store.FinishJob(ctx, job, f.Assignment.Command.OperationID, "rejected", "no_attempt", "rejected", "policy_unavailable", nil, true, "no_request_sent")
		done <- e
	}()
	time.Sleep(250 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; !errors.Is(e, jobs.ErrLeaseLost) {
		t.Fatal("stale waited result committed", e)
	}
	op, e := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if e != nil || op.FinishedAt != nil {
		t.Fatal("stale result released", op.State, e)
	}
}
func TestAssignmentMigrationRollbackRefusesDurableIdentity(t *testing.T) {
	f := assignmentFixture(t)
	f.AdmitAndClaim(t)
	raw, e := os.ReadFile("../../migrations/000019_distribution_operations.down.sql")
	if e != nil {
		t.Fatal(e)
	}
	tx, e := f.Pool.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(context.Background(), string(raw)); e == nil {
		t.Fatal("used operation migration dropped")
	}
	_ = tx.Rollback(context.Background())
	if _, e = f.Store.Operation(context.Background(), f.Scope, f.Assignment.Command.OperationID); e != nil {
		t.Fatal("rollback erased receipt", e)
	}
}
func TestAssignmentIdentityRecheckedAfterPreparation(t *testing.T) {
	for _, kind := range []string{"target", "actor"} {
		t.Run(kind, func(t *testing.T) {
			f := assignmentFixture(t)
			if kind == "actor" {
				id := uuid.New()
				if _, e := f.Pool.Exec(context.Background(), `INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,1)`, f.Assignment.Scope.BindingID, id); e != nil {
					t.Fatal(e)
				}
				f.Assignment.Command.Actor = Actor{Kind: "user", TeamOSUserID: &id, CRMUserID: ptr(int64(1))}
				f.CRM.crmFake.lead = amocrm.DistributionLead{ID: 10, PipelineID: 20, StatusID: 30, ResponsibleUserID: 1}
			}
			f.CRM.PrepareHook = func() {
				if kind == "target" {
					u := f.CRM.users[2]
					u.Rights.IsActive = ptr(false)
					f.CRM.users[2] = u
				} else {
					u := f.CRM.users[1]
					u.Rights.Leads = map[string]string{"view": "D"}
					f.CRM.users[1] = u
				}
			}
			job := f.AdmitAndClaim(t)
			if _, e := f.Worker().Handler(context.Background(), job); e != nil {
				t.Fatal(e)
			}
			if f.CRM.Calls != 0 {
				t.Fatal("identityrevokedduringbudgetwaitignored")
			}
		})
	}
}
func TestAssignmentKeyReceiptPrecedesGlobalOperationLookup(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	b := f.Assignment
	b.Command.OperationID = uuid.New()
	b.Command.DecisionID = uuid.New()
	b.Command.Expected.LeadID = 11
	keyB, keyA := uuid.NewString(), uuid.NewString()
	rb, _, e := f.Store.Admit(ctx, f.Scope, keyB, b)
	if e != nil {
		t.Fatal(e)
	}
	ra, _, e := f.Store.Admit(ctx, f.Scope, keyA, f.Assignment)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = f.Store.Admit(ctx, f.Scope, keyA, b); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatal("existingkey reboundtootheroperation", e)
	}
	for _, pair := range []struct {
		key     string
		command Assignment
		receipt Receipt
	}{{keyA, f.Assignment, ra}, {keyB, b, rb}} {
		r, replayed, e := f.Store.Admit(ctx, f.Scope, pair.key, pair.command)
		if e != nil || !replayed || r != pair.receipt {
			t.Fatal("originalreceiptchanged", r, replayed, e)
		}
	}
}

// A no-PATCH result still confirms a business turn. SQL contention must not
// turn a grant that expired at shift end into a cursor-advancing result.
func TestAssignmentNoChangeSettlementRechecksGrantAndCancellation(t *testing.T) {
	for _, kind := range []string{"expired_operation_lock", "expired_guard_lock", "cancel", "binding_revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := assignmentFixture(t)
			ctx := context.Background()
			job := f.AdmitAndClaim(t)
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if kind == "expired_guard_lock" {
				_, err = tx.Exec(ctx, `SELECT operation_id FROM distribution_lead_guards WHERE operation_id=$1 FOR UPDATE`, f.Assignment.Command.OperationID)
			} else {
				_, err = tx.Exec(ctx, `SELECT id FROM distribution_operations WHERE id=$1 FOR UPDATE`, f.Assignment.Command.OperationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "cancel" {
				_, err = tx.Exec(ctx, `UPDATE distribution_operations SET cancel_requested_at=clock_timestamp() WHERE id=$1`, f.Assignment.Command.OperationID)
			}
			if kind == "binding_revoked" {
				_, err = tx.Exec(ctx, `UPDATE distribution_bindings SET state='revoked' WHERE id=$1`, f.Assignment.Scope.BindingID)
			}
			if err != nil {
				t.Fatal(err)
			}
			grant := time.Now().Add(150 * time.Millisecond)
			if kind == "cancel" || kind == "binding_revoked" {
				grant = time.Now().Add(3 * time.Second)
			}
			done := make(chan error, 1)
			snapshot := snapshotOf(f.CRM.Lead, time.Now())
			go func() {
				_, err := f.Store.FinishNoChange(ctx, job, f.Assignment.Command.OperationID, "already_target", &snapshot, grant)
				done <- err
			}()
			// Confirm the transaction reached a lock wait before releasing it. This
			// ensures the regression exercises post-wait checks rather than admission.
			deadline := time.Now().Add(2 * time.Second)
			for {
				var waiting bool
				err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%distribution_%')`).Scan(&waiting)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("settlement never waited for lock")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if kind == "expired_operation_lock" || kind == "expired_guard_lock" {
				time.Sleep(time.Until(grant.Add(30 * time.Millisecond)))
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			want := ErrStaleDecision
			if kind == "binding_revoked" {
				want = ErrDenied
			}
			if !errors.Is(err, want) {
				t.Fatalf("stale no-change result settled: %v want %v", err, want)
			}
			var results, outbox, guards int
			err = f.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM distribution_operation_results),(SELECT count(*) FROM distribution_result_outbox),(SELECT count(*) FROM distribution_lead_guards)`).Scan(&results, &outbox, &guards)
			if err != nil || results != 1 || outbox != 1 || guards != 1 {
				t.Fatal("expired business turn emitted/released", results, outbox, guards, err)
			}
			op, err := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
			if err != nil || op.FinishedAt != nil || op.State != "queued" {
				t.Fatal(op.State, err)
			}
		})
	}
}
