package crmevents

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// Postgres is the service-owned persistence adapter. It has no foreign DSN or
// repository; all transactions, leases and inbox effects stay in this database.
type Postgres struct {
	pool *pgxpool.Pool
	cfg  Config
}

func NewPostgres(pool *pgxpool.Pool, cfg Config) *Postgres {
	return &Postgres{pool: pool, cfg: normalizeConfig(cfg)}
}

var _ Repository = (*Postgres)(nil)

// New is the composition convenience wrapper; application logic uses Repository.
func New(pool *pgxpool.Pool, policy serviceapi.Policy, gateway serviceapi.Gateway, cfg Config) *Service {
	return NewWithRepository(NewPostgres(pool, cfg), policy, gateway, cfg)
}
func (s *Postgres) Apply(ctx context.Context, cmd serviceapi.Command, p serviceapi.Principal) (serviceapi.Operation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO event_sources(installation_id,integration_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.InstallationID, p.IntegrationID)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	var integration uuid.UUID
	err = tx.QueryRow(ctx, `SELECT integration_id FROM event_sources WHERE installation_id=$1 FOR UPDATE`, p.InstallationID).Scan(&integration)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	if integration != p.IntegrationID {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.PermissionDenied, "source scope mismatch")
	}
	hash := commandHash(cmd, p)
	var previous []byte
	var operationID uuid.UUID
	// In-horizon replay returns the original operation. After receipt GC a
	// tombstone rejects the same command_id so it cannot become a new accept.
	err = tx.QueryRow(ctx, `SELECT payload_hash,operation_id FROM event_inbox WHERE installation_id=$1 AND command_id=$2`, p.InstallationID, cmd.CommandID).Scan(&previous, &operationID)
	if err == nil {
		if string(previous) != string(hash) {
			return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "command id already used with different content")
		}
		op, e := readOperation(ctx, tx, p.InstallationID, operationID)
		return op, e
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.Operation{}, err
	}
	if cmd.Kind == "backfill" && (cmd.To > s.cfg.Now().Unix() || cmd.From < s.cfg.Now().Add(-time.Duration(cmd.RetentionDays)*24*time.Hour).Unix()) {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.InvalidArgument, "backfill must be within past retention horizon")
	}
	operationID, err = uuid.Parse(cmd.CommandID)
	if err != nil {
		operationID = uuid.NewSHA1(p.InstallationID, []byte(cmd.CommandID))
	}
	state := "accepted"
	if cmd.Kind == "disable" {
		state = "completed"
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_operations(id,installation_id,actor_id,kind,status,requested_from,requested_to) VALUES($1,$2,$3,$4,$5,$6,$7)`, operationID, p.InstallationID, p.ActorID, cmd.Kind, state, optionalTime(cmd.From), optionalTime(cmd.To))
	if err != nil {
		return serviceapi.Operation{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_inbox(installation_id,command_id,payload_hash,operation_id) VALUES($1,$2,$3,$4)`, p.InstallationID, cmd.CommandID, hash, operationID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "command id already used or expired")
		}
		return serviceapi.Operation{}, err
	}
	enabled := cmd.Kind != "disable"
	if cmd.Kind == "backfill" {
		var active bool
		if err = tx.QueryRow(ctx, `SELECT coalesce(bool_or(enabled),false) FROM event_consumers WHERE installation_id=$1`, p.InstallationID).Scan(&active); err != nil {
			return serviceapi.Operation{}, err
		}
		if !active {
			return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "enable source before backfill")
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO event_consumers(installation_id,consumer,enabled) VALUES($1,'activity',$2) ON CONFLICT(installation_id,consumer) DO UPDATE SET enabled=excluded.enabled,updated_at=now()`, p.InstallationID, enabled)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	if !enabled {
		_, err = tx.Exec(ctx, `UPDATE event_sources SET state='disabled',lease_token=lease_token+1,lease_until=NULL,updated_at=now() WHERE installation_id=$1`, p.InstallationID)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE event_jobs SET status='paused',updated_at=now() WHERE installation_id=$1 AND status IN ('queued','running','retry')`, p.InstallationID)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE event_operations SET status='paused',updated_at=now() WHERE installation_id=$1 AND status IN ('accepted','running','retry')`, p.InstallationID)
		}
	} else {
		_, err = tx.Exec(ctx, `UPDATE event_sources SET initial_hours=$2,retention_days=$3,state='pending',error_code='',updated_at=now() WHERE installation_id=$1`, p.InstallationID, cmd.InitialDays*24, cmd.RetentionDays)
		if err == nil && cmd.Kind != "backfill" {
			_, err = tx.Exec(ctx, `UPDATE event_jobs SET status='queued',attempts=0,page=CASE WHEN error_code IN ('pagination_unstable','page_limit_exceeded') THEN 1 ELSE page END,scan_pass=CASE WHEN error_code IN ('pagination_unstable','page_limit_exceeded') THEN 1 ELSE scan_pass END,pass_digest=CASE WHEN error_code IN ('pagination_unstable','page_limit_exceeded') THEN '' ELSE pass_digest END,previous_digest=CASE WHEN error_code IN ('pagination_unstable','page_limit_exceeded') THEN '' ELSE previous_digest END,error_code='',run_after=now() WHERE installation_id=$1 AND status = 'paused'`, p.InstallationID)
		}
		if err == nil {
			from, to := time.Unix(cmd.From, 0).UTC(), time.Unix(cmd.To, 0).UTC()
			kind := "backfill"
			priority := 100
			if cmd.Kind != "backfill" {
				kind = "current"
				priority = 10
				to = s.cfg.Now().UTC().Truncate(time.Second)
				var through *time.Time
				err = tx.QueryRow(ctx, `SELECT continuous_to FROM event_sources WHERE installation_id=$1`, p.InstallationID).Scan(&through)
				from = to.Add(-time.Duration(cmd.InitialDays) * 24 * time.Hour)
				if through != nil {
					from = through.Add(-s.cfg.Overlap)
				}
				// Attach an explicit sync to existing durable work, preserving the fixed cursor.
				var existing uuid.UUID
				e := tx.QueryRow(ctx, `SELECT id FROM event_jobs WHERE installation_id=$1 AND kind='current' AND status IN ('queued','running','retry','paused') LIMIT 1`, p.InstallationID).Scan(&existing)
				if e == nil {
					_, err = tx.Exec(ctx, `INSERT INTO event_operation_jobs(operation_id,job_id) VALUES($1,$2)`, operationID, existing)
					if err == nil {
						op, e := readOperation(ctx, tx, p.InstallationID, operationID)
						if e != nil {
							return op, e
						}
						return op, tx.Commit(ctx)
					}
				} else if !errors.Is(e, pgx.ErrNoRows) {
					err = e
				}
			}
			if err == nil {
				jobID := uuid.New()
				_, err = tx.Exec(ctx, `INSERT INTO event_jobs(id,installation_id,operation_id,kind,priority,window_from,window_to,target_to) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, jobID, p.InstallationID, operationID, kind, priority, from, minTime(from.Add(s.cfg.Window), to), to)
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO event_operation_jobs(operation_id,job_id) VALUES($1,$2)`, operationID, jobID)
				}
			}
		}
	}
	if err != nil {
		return serviceapi.Operation{}, err
	}
	op, err := readOperation(ctx, tx, p.InstallationID, operationID)
	if err != nil {
		return op, err
	}
	return op, tx.Commit(ctx)
}
func optionalTime(epoch int64) any {
	if epoch == 0 {
		return nil
	}
	return time.Unix(epoch, 0).UTC()
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type rowReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readOperation(ctx context.Context, q rowReader, installationID, opID uuid.UUID) (serviceapi.Operation, error) {
	var op serviceapi.Operation
	err := q.QueryRow(ctx, `SELECT o.id::text,coalesce(i.command_id,''),o.status,o.error_code,o.processed,o.inserted,o.updated,o.deduplicated FROM event_operations o LEFT JOIN event_inbox i ON i.operation_id=o.id WHERE o.installation_id=$1 AND o.id=$2`, installationID, opID).Scan(&op.ID, &op.CommandID, &op.State, &op.ErrorCode, &op.Processed, &op.Inserted, &op.Updated, &op.Deduplicated)
	if errors.Is(err, pgx.ErrNoRows) {
		err = serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	return op, err
}

func (s *Postgres) Operation(ctx context.Context, p serviceapi.Principal, id uuid.UUID) (serviceapi.Operation, error) {
	var ownScope bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM event_sources WHERE installation_id=$1 AND integration_id=$2)`, p.InstallationID, p.IntegrationID).Scan(&ownScope); err != nil {
		return serviceapi.Operation{}, err
	}
	if !ownScope {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	return readOperation(ctx, s.pool, p.InstallationID, id)
}
