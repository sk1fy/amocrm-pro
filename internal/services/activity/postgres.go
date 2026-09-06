package activity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"time"
)

// Postgres receives only the Activity-owned pool. External identifiers are
// opaque references; no query touches Core or CRM Events tables.
type Postgres struct{ pool *pgxpool.Pool }

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (s *Postgres) Settings(ctx context.Context, scope serviceapi.Scope) (serviceapi.Settings, error) {
	var settings serviceapi.Settings
	err := s.pool.QueryRow(ctx, `SELECT initial_days, retention_days FROM settings WHERE installation_id=$1 AND integration_id=$2`, scope.InstallationID, scope.IntegrationID).Scan(&settings.InitialDays, &settings.RetentionDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return Defaults(), nil
	}
	return settings, err
}

func (s *Postgres) Configure(ctx context.Context, p serviceapi.Principal, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	id, err := uuid.Parse(c.CommandID)
	if err != nil || id == uuid.Nil {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.InvalidArgument, "command_id must be a UUID")
	}
	if err := ValidateSettings(c.Settings); err != nil {
		return serviceapi.Operation{}, err
	}
	canonical, _ := json.Marshal(struct {
		Scope    serviceapi.Scope
		Actor    int64
		Settings serviceapi.Settings
	}{p.Scope, p.ActorID, c.Settings})
	hash := sha256.Sum256(canonical)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.Operation{}, err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `INSERT INTO command_receipts(command_id,installation_id,integration_id,actor_id,request_hash) VALUES($1,$2,$3,$4,$5) ON CONFLICT(command_id) DO NOTHING`, id, p.InstallationID, p.IntegrationID, p.ActorID, hash[:])
	if err != nil {
		return serviceapi.Operation{}, err
	}
	if tag.RowsAffected() == 0 {
		var existing []byte
		if err := tx.QueryRow(ctx, `SELECT request_hash FROM command_receipts WHERE command_id=$1`, id).Scan(&existing); err != nil {
			return serviceapi.Operation{}, err
		}
		if !bytes.Equal(hash[:], existing) {
			return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO settings(installation_id,integration_id,initial_days,retention_days) VALUES($1,$2,$3,$4) ON CONFLICT(installation_id) DO UPDATE SET initial_days=EXCLUDED.initial_days,retention_days=EXCLUDED.retention_days,updated_at=now() WHERE settings.integration_id=EXCLUDED.integration_id`, p.InstallationID, p.IntegrationID, c.Settings.InitialDays, c.Settings.RetentionDays); err != nil {
			return serviceapi.Operation{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return serviceapi.Operation{}, err
	}
	return serviceapi.Operation{ID: c.CommandID, CommandID: c.CommandID, State: serviceapi.OperationSucceeded}, nil
}

func (s *Postgres) Operation(ctx context.Context, p serviceapi.Principal, id string) (serviceapi.Operation, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	var marker int
	err = s.pool.QueryRow(ctx, `SELECT 1 FROM command_receipts WHERE command_id=$1 AND installation_id=$2 AND integration_id=$3 AND actor_id=$4`, parsed, p.InstallationID, p.IntegrationID, p.ActorID).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return serviceapi.Operation{ID: id, CommandID: id, State: serviceapi.OperationSucceeded}, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
