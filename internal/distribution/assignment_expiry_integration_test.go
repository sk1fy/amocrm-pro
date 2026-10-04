package distribution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignedExpiredAssignmentFencesLostAdmissionAndLateReplay(t *testing.T) {
	f := assignmentFixture(t)
	h := assignmentHTTP(t, f)
	ctx := context.Background()
	key := uuid.NewString()
	path := "/internal/v1/distribution/assignments/expire"
	h.call(t, f.Scope, "POST", path, key, f.Assignment, 409)
	f.Assignment.Command.ValidUntil = time.Now().Add(-time.Minute)
	// Safe expiry remains possible after the scope has stopped accepting work.
	if _, err := f.Pool.Exec(ctx, `UPDATE distribution_bindings SET state='revoked'; UPDATE integration_services SET enabled=false; UPDATE distribution_admin_pauses SET paused=true`); err != nil {
		t.Fatal(err)
	}
	op := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", path, key, f.Assignment, 200))
	if op.State != "rejected" || op.ExternalEffectState != "no_attempt" || !op.ResolutionEvidence.GuardReleasable || op.ResolutionEvidence.Kind != "no_request_sent" || op.ResultVersion != 1 || op.Error == nil || op.Error.Code != "decision_expired" {
		t.Fatalf("missing terminal expiry evidence: %+v", op)
	}
	// Losing the expiry response and retrying must not create another result.
	again := decodeHTTPAssignmentOperation(t, h.call(t, f.Scope, "POST", path, key, f.Assignment, 200))
	if again.ResultVersion != op.ResultVersion {
		t.Fatal("expiry replay changed result version")
	}
	h.call(t, f.Scope, "POST", "/internal/v1/distribution/assignments", key, f.Assignment, 202)
	changed := f.Assignment
	changed.Command.ValidUntil = time.Now().Add(time.Minute)
	h.call(t, f.Scope, "POST", "/internal/v1/distribution/assignments", key, changed, 409)
	h.call(t, f.Scope, "POST", path, key, changed, 409)
	foreign := f.Scope
	foreign.CompanyID = uuid.New()
	h.call(t, foreign, "POST", path, key, f.Assignment, 401)
	wrongBinding := f.Assignment
	wrongBinding.Scope.AccountID++
	wrongBinding.Command.OperationID = uuid.New()
	wrongBinding.Command.DecisionID = uuid.New()
	h.call(t, f.Scope, "POST", path, uuid.NewString(), wrongBinding, 403)
	var executable, guards, attempts, results, outbox, cancelled int
	err := f.Pool.QueryRow(ctx, `SELECT
	 (SELECT count(*) FROM jobs WHERE status IN ('queued','processing','retry')),
	 (SELECT count(*) FROM distribution_lead_guards),
	 (SELECT count(*) FROM distribution_operation_attempts),
	 (SELECT count(*) FROM distribution_operation_results),
	 (SELECT count(*) FROM distribution_result_outbox),
	 (SELECT count(*) FROM jobs WHERE status='cancelled')`).Scan(&executable, &guards, &attempts, &results, &outbox, &cancelled)
	if err != nil || executable != 0 || guards != 0 || attempts != 0 || results != 1 || outbox != 1 || cancelled != 1 || f.CRM.Calls != 0 {
		t.Fatal("expiry had effects or duplicate results", executable, guards, attempts, results, outbox, cancelled, err)
	}
}

func TestExpiredAssignmentPreservesExistingUnknownAndUnrelatedGuard(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	f.CRM.Mode = "timeout_applied"
	job := f.AdmitAndClaim(t)
	if _, err := f.Worker().Handler(ctx, job); err != nil {
		t.Fatal(err)
	}
	before, err := f.Store.Operation(ctx, f.Scope, f.Assignment.Command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	// A lookup through expiry cannot rewrite evidence for an admitted operation.
	op, replayed, err := f.Store.ExpireAssignment(ctx, f.Scope, uuid.NewString(), f.Assignment)
	if err != nil || !replayed || op.ResultVersion != before.ResultVersion || op.State != "outcome_unknown" || op.ResolutionEvidence.GuardReleasable {
		t.Fatal("existing unknown was settled", op, err)
	}
	next := f.Assignment
	next.Command.OperationID, next.Command.DecisionID = uuid.New(), uuid.New()
	next.Command.ValidUntil = time.Now().Add(-time.Minute)
	if _, _, err := f.Store.ExpireAssignment(ctx, f.Scope, uuid.NewString(), next); err != nil {
		t.Fatal(err)
	}
	var holder uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT operation_id FROM distribution_lead_guards WHERE account_id=$1 AND lead_id=$2`, next.Scope.AccountID, next.Command.Expected.LeadID).Scan(&holder); err != nil || holder != before.OperationID || f.CRM.Calls != 1 {
		t.Fatal("expiry released another operation guard", holder, err)
	}
}

func TestExpiredAssignmentConcurrentAdmissionAndExpiry(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	f.Assignment.Command.ValidUntil = time.Now().Add(-time.Minute)
	key := uuid.NewString()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(expire bool) {
			defer wg.Done()
			if expire {
				_, _, err := f.Store.ExpireAssignment(ctx, f.Scope, key, f.Assignment)
				errs <- err
			} else {
				_, _, err := f.Store.Admit(ctx, f.Scope, key, f.Assignment)
				if errors.Is(err, ErrStaleDecision) {
					err = nil
				}
				errs <- err
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var operations, effects int
	if err := f.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM distribution_operations), (SELECT count(*) FROM distribution_lead_guards)+(SELECT count(*) FROM jobs WHERE status<>'cancelled')`).Scan(&operations, &effects); err != nil || operations != 1 || effects != 0 {
		t.Fatal("admission/expiry race", operations, effects, err)
	}
}

func TestAssignmentAdmissionRechecksDeadlineAfterLeadLock(t *testing.T) {
	f := assignmentFixture(t)
	ctx := context.Background()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('123:10',0))`); err != nil {
		t.Fatal(err)
	}
	f.Assignment.Command.ValidUntil = time.Now().Add(3 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, _, err := f.Store.Admit(ctx, f.Scope, uuid.NewString(), f.Assignment)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("admission did not wait for the lead lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(time.Until(f.Assignment.Command.ValidUntil.Add(30 * time.Millisecond)))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrStaleDecision) {
		t.Fatal("expired admission passed lock wait", err)
	}
}
