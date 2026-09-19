package admincommand

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"time"
)

// A successful reservation detaches one connection from the work pool. At most
// GlobalChecks such sessions exist. This reserves capacity for the OAuth and
// completion transactions even when the work pool has a single connection.
// Waiters release their pool connection before waiting; no pool starvation.
func (w *WorkerExecutor) admitCheck(ctx context.Context, id uuid.UUID) (func(), error) {
	global := w.GlobalChecks
	if global < 1 {
		global = 4
	}
	for {
		conn, err := w.pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		var integration uuid.UUID
		err = conn.QueryRow(ctx, `SELECT integration_id FROM installations WHERE id=$1`, id).Scan(&integration)
		if err != nil {
			conn.Release()
			return nil, err
		}
		lock := func(key string) (bool, error) {
			var ok bool
			err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, key).Scan(&ok)
			return ok, err
		}
		account, err := lock("admin-check-installation:" + id.String())
		integrationOK := false
		if err == nil && account {
			for slot := 0; slot < 2; slot++ {
				integrationOK, err = lock(fmt.Sprintf("admin-check-integration:%s:%d", integration, slot))
				if err != nil || integrationOK {
					break
				}
			}
		}
		globalOK := false
		if err == nil && integrationOK {
			for slot := 0; slot < global; slot++ {
				globalOK, err = lock(fmt.Sprintf("admin-check-global:%d", slot))
				if err != nil || globalOK {
					break
				}
			}
		}
		if err == nil && globalOK {
			session := conn.Hijack()
			return func() { closeCheckSession(session) }, nil
		}
		unlockCheckConnection(conn)
		conn.Release()
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func closeCheckSession(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}
