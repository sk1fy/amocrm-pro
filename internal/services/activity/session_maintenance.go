package activity

import (
	"context"
	"log/slog"
	"time"
)

// RunSessionMaintenance bounds each batch. The legacy watchdog closes only work
// tasks after eight hours at the observation time; it never fabricates a duration.
func (s *Postgres) RunSessionMaintenance(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		batch, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := s.maintainSessions(batch); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "activity session maintenance failed", "error", err)
		}
		cancel()
	}
}
func (s *Postgres) maintainSessions(ctx context.Context) error {
	// A row lock serializes the close with starts/stops; the final predicate is
	// rechecked after waiting so an already stopped interval is never rewritten.
	_, err := s.pool.Exec(ctx, `WITH candidates AS (
 SELECT id FROM work_sessions WHERE kind='work' AND stopped_at IS NULL AND started_at<now()-interval '8 hours'
 ORDER BY started_at LIMIT 100 FOR UPDATE SKIP LOCKED
 ) UPDATE work_sessions SET stopped_at=greatest(started_at,now()) WHERE id IN(SELECT id FROM candidates) AND stopped_at IS NULL`)
	if err != nil {
		return err
	}
	// Keep a complete rolling month, including intervals crossing the boundary.
	_, err = s.pool.Exec(ctx, `WITH expired AS (SELECT id FROM work_sessions WHERE stopped_at<now()-interval '32 days' ORDER BY stopped_at LIMIT 1000 FOR UPDATE SKIP LOCKED) DELETE FROM work_sessions WHERE id IN(SELECT id FROM expired)`)
	return err
}
