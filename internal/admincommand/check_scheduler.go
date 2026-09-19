package admincommand

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"os"
	"strconv"
	"time"
)

const defaultCheckInterval = time.Hour
const maxCheckInterval = 72 * time.Minute

type CheckConfig struct {
	Enabled     bool
	Interval    time.Duration
	Tick        time.Duration
	Global      int
	Integration string
	Account     int64
	Percent     int
}

func LoadCheckConfig() (CheckConfig, error) {
	c := CheckConfig{Interval: defaultCheckInterval, Tick: 30 * time.Second, Global: 4, Percent: 100}
	var err error
	if v := os.Getenv("ADMIN_CONNECTION_CHECKS_ENABLED"); v != "" {
		c.Enabled, err = strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("invalid ADMIN_CONNECTION_CHECKS_ENABLED")
		}
	}
	for key, target := range map[string]*time.Duration{"ADMIN_CONNECTION_CHECK_INTERVAL": &c.Interval, "ADMIN_CONNECTION_CHECK_TICK": &c.Tick} {
		if v := os.Getenv(key); v != "" {
			*target, err = time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
		}
	}
	for key, target := range map[string]*int{"ADMIN_CONNECTION_CHECK_CONCURRENCY": &c.Global, "ADMIN_CONNECTION_CHECK_PERCENT": &c.Percent} {
		if v := os.Getenv(key); v != "" {
			*target, err = strconv.Atoi(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
		}
	}
	c.Integration = os.Getenv("ADMIN_CONNECTION_CHECK_INTEGRATION")
	if c.Integration != "" {
		if _, err := uuid.Parse(c.Integration); err != nil {
			return c, fmt.Errorf("invalid ADMIN_CONNECTION_CHECK_INTEGRATION")
		}
	}
	if v := os.Getenv("ADMIN_CONNECTION_CHECK_ACCOUNT"); v != "" {
		c.Account, err = strconv.ParseInt(v, 10, 64)
		if err != nil || c.Account < 1 {
			return c, fmt.Errorf("invalid ADMIN_CONNECTION_CHECK_ACCOUNT")
		}
	}
	if c.Interval < time.Minute || c.Interval > maxCheckInterval || c.Tick < time.Second || c.Tick > time.Minute || c.Global < 1 || c.Global > 32 || c.Percent < 0 || c.Percent > 100 {
		return c, fmt.Errorf("invalid connection check scheduling limits")
	}
	return c, nil
}

type CheckScheduler struct {
	Pool    *pgxpool.Pool
	Store   *Store
	Config  CheckConfig
	Metrics *CheckMetrics
	Logger  *slog.Logger
}

func (s *CheckScheduler) Run(ctx context.Context) {
	if s.Metrics != nil && s.Config.Enabled {
		s.Metrics.Enabled.Set(1)
	}
	tick := time.NewTicker(s.Config.Tick)
	defer tick.Stop()
	for {
		if err := s.Tick(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil && s.Logger != nil {
			s.Logger.Warn("connection check scheduling failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *CheckScheduler) Tick(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pooled, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer closeCheckSession(conn)
	if s.Metrics != nil {
		if err := s.Metrics.Refresh(ctx, conn, now); err != nil {
			return err
		}
	}
	if !s.Config.Enabled {
		return nil
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('admin-check-scheduler',0))`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}

	// Seeding is durable and deterministic, so a restart or another replica never
	// restarts the spread. Only active scoped installations are admitted.
	_, err = conn.Exec(ctx, `INSERT INTO installation_check_schedule(installation_id,next_check_at)
 SELECT i.id,$1::timestamptz+make_interval(secs=>(('x'||substr(md5(i.id::text),1,8))::bit(32)::bigint % $2)::int)
 FROM installations i JOIN integrations ig ON ig.id=i.integration_id
 WHERE i.status='active' AND ig.status='active' AND ($3='' OR i.integration_id::text=$3)
 AND ($4::bigint=0 OR i.account_id=$4) AND (('x'||substr(md5(i.id::text),1,8))::bit(32)::bigint %100)<$5
 AND NOT EXISTS(SELECT 1 FROM installation_check_schedule sch WHERE sch.installation_id=i.id)
 ORDER BY i.id LIMIT 100
 ON CONFLICT DO NOTHING`, now, int(s.Config.Interval.Seconds()), s.Config.Integration, s.Config.Account, s.Config.Percent)
	if err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT i.id,sch.next_check_at
 FROM installations i JOIN integrations ig ON ig.id=i.integration_id
 JOIN installation_check_schedule sch ON sch.installation_id=i.id
 LEFT JOIN oauth_credentials oc ON oc.installation_id=i.id
 LEFT JOIN installation_checks ck ON ck.installation_id=i.id AND ck.credential_version=COALESCE(oc.token_version,0) AND ck.installation_status=i.status
 WHERE i.status='active' AND ig.status='active' AND (oc.lease_until IS NULL OR oc.lease_until<=$1)
 AND sch.next_check_at<=$1 AND (ck.next_check_at IS NULL OR ck.next_check_at<=$1)
 AND NOT EXISTS(SELECT 1 FROM admin_commands c WHERE c.installation_id=i.id AND c.state IN ('accepted','pending','running'))
 AND ($2='' OR i.integration_id::text=$2) AND ($3::bigint=0 OR i.account_id=$3)
 AND (('x'||substr(md5(i.id::text),1,8))::bit(32)::bigint %100)<$4
 ORDER BY sch.next_check_at,i.id LIMIT $5`, now, s.Config.Integration, s.Config.Account, s.Config.Percent, s.Config.Global*2)
	if err != nil {
		return err
	}
	type candidate struct {
		id  uuid.UUID
		due time.Time
	}
	items := []candidate{}
	for rows.Next() {
		var i candidate
		if err := rows.Scan(&i.id, &i.due); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		if s.Metrics != nil && now.Sub(item.due) > 0 {
			s.Metrics.Stale.Inc()
		}
		key := fmt.Sprintf("connection-check:%s:%d", item.id, now.Unix()/int64(s.Config.Interval.Seconds()))
		receipt, err := s.Store.Execute(ctx, "automation:connection-checks", key, Request{TargetType: "installation", TargetID: item.id.String(), Command: "check", Payload: json.RawMessage(`{}`)})
		if err != nil {
			continue
		}
		if receipt.State == "pending" || receipt.State == "succeeded" {
			if _, err := conn.Exec(ctx, `UPDATE installation_check_schedule SET next_check_at=$2 WHERE installation_id=$1`, item.id, now.Add(checkDelay(item.id.String(), now, s.Config.Interval, 0))); err != nil {
				return err
			}
		}
	}
	if s.Metrics != nil {
		return s.Metrics.Refresh(ctx, conn, now)
	}
	return nil
}
func unlockCheckConnection(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		_ = conn.Conn().Close(ctx)
	}
}
