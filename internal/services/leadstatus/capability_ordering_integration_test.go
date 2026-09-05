package leadstatus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestCapabilityRevocationWaitsForAuthorizedMutation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	principal := widgetPrincipal(t, pool, 304, 74)
	job := admitAndClaimLeadStatus(t, pool, principal, LeadStatusCommand{
		LeadID: 5002, PipelineID: 6002, StatusID: 7002,
	})
	execution := NewExecutionStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	revoker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revokerPID := revoker.Conn().PgConn().PID()
	var workers sync.WaitGroup
	defer func() {
		// Also releases a blocked callback/query when an assertion fails.
		cancel()
		workers.Wait()
		revoker.Release()
	}()

	entered := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		mutationDone <- execution.WithMutationAuthorization(ctx, job, LeadSetStatusJobType, leadResourceType,
			func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
	}()
	select {
	case <-entered:
	case err := <-mutationDone:
		t.Fatalf("authorization finished without invoking the mutation: %v", err)
	case <-ctx.Done():
		t.Fatal("mutation did not reach the callback")
	}

	revocationDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		tag, err := revoker.Exec(ctx, `UPDATE integration_services SET enabled=false
			WHERE integration_id=$1 AND service_code='lead-status'`, principal.IntegrationID)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("revocation did not update exactly one grant")
		}
		revocationDone <- err
	}()

	// Observe the actual blocked database session rather than assuming a
	// goroutine has issued its UPDATE after an arbitrary scheduling delay.
	for {
		select {
		case err := <-revocationDone:
			t.Fatalf("revocation completed before the mutation released its guard: %v", err)
		default:
		}
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted
		)`, revokerPID).Scan(&waiting); err != nil {
			t.Fatalf("observe revocation lock wait: %v", err)
		}
		if waiting {
			break
		}
	}
	close(release)
	if err := <-mutationDone; err != nil {
		t.Fatalf("authorized mutation failed: %v", err)
	}
	if err := <-revocationDone; err != nil {
		t.Fatalf("revocation failed after mutation commit: %v", err)
	}

	called := false
	err = execution.WithMutationAuthorization(ctx, job, LeadSetStatusJobType, leadResourceType,
		func(context.Context) error {
			called = true
			return nil
		})
	if !errors.Is(err, ErrExecutionNotAuthorized) || called {
		t.Fatalf("mutation after revocation: error=%v callback_called=%t", err, called)
	}
}
