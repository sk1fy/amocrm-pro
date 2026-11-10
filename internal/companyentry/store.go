package companyentry

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type Store struct{ Pool *pgxpool.Pool }

func (s *Store) Active(ctx context.Context, p widgetauth.Principal) error {
	var exists bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installations i JOIN integrations n ON n.id=i.integration_id WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3 AND i.status='active' AND n.status='active')`, p.InstallationID, p.IntegrationID, p.AccountID).Scan(&exists)
	if err == nil && !exists {
		return ErrDenied
	}
	return err
}
func (s *Store) Associated(ctx context.Context, account int64) (uuid.UUID, error) {
	var company uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT company_id FROM teamos_account_companies WHERE account_id=$1`, account).Scan(&company)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	return company, err
}
func (s *Store) Associate(ctx context.Context, account int64, company uuid.UUID) error {
	if account <= 0 || company == uuid.Nil {
		return ErrConflict
	}
	tag, err := s.Pool.Exec(ctx, `INSERT INTO teamos_account_companies(account_id,company_id) VALUES($1,$2) ON CONFLICT(account_id) DO UPDATE SET observed_at=now() WHERE teamos_account_companies.company_id=EXCLUDED.company_id`, account, company)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return ErrConflict
	}
	if err == nil && tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}
