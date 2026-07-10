package installations

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("installation not found")

type Installation struct {
	ID             uuid.UUID
	IntegrationID  uuid.UUID
	AccountID      int64
	AccountDomain  string
	Status         string
	WebhookKeyHash []byte
	WebhookStatus  string
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) FindActiveByWebhookKeyHash(ctx context.Context, keyHash []byte) (Installation, error) {
	var installation Installation
	err := s.pool.QueryRow(ctx, `
		SELECT id, integration_id, account_id, account_domain, status,
			webhook_key_hash, webhook_status
		FROM installations
		WHERE webhook_key_hash = $1
		  AND status = 'active'
		  AND webhook_status <> 'disabled'`, keyHash,
	).Scan(
		&installation.ID,
		&installation.IntegrationID,
		&installation.AccountID,
		&installation.AccountDomain,
		&installation.Status,
		&installation.WebhookKeyHash,
		&installation.WebhookStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, ErrNotFound
	}
	if err != nil {
		return Installation{}, fmt.Errorf("find installation by webhook key: %w", err)
	}
	return installation, nil
}

func (s *Store) FindActiveByTenant(ctx context.Context, clientID string, accountID int64) (Installation, error) {
	var installation Installation
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, i.integration_id, i.account_id, i.account_domain, i.status,
			i.webhook_key_hash, i.webhook_status
		FROM installations i
		JOIN integrations product ON product.id = i.integration_id
		WHERE product.client_id = $1 AND i.account_id = $2
		  AND product.status = 'active' AND i.status = 'active'`, clientID, accountID,
	).Scan(
		&installation.ID,
		&installation.IntegrationID,
		&installation.AccountID,
		&installation.AccountDomain,
		&installation.Status,
		&installation.WebhookKeyHash,
		&installation.WebhookStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Installation{}, ErrNotFound
	}
	if err != nil {
		return Installation{}, fmt.Errorf("find installation by tenant: %w", err)
	}
	return installation, nil
}
