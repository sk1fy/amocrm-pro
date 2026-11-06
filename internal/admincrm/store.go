package admincrm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store selects the reading connection. It reads only identifiers and status
// columns; credentials stay with the OAuth token provider.
type Store struct {
	Pool    *pgxpool.Pool
	Timeout time.Duration
}

// ResolveConnection picks the active, non-fixture installation of an active
// integration with stored OAuth credentials, preferring the most recently
// refreshed credentials. The order is deterministic for equal timestamps.
func (s *Store) ResolveConnection(ctx context.Context, accountID int64) (Connection, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var connection Connection
	err := s.Pool.QueryRow(ctx, `SELECT i.id, i.integration_id, ig.code
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
JOIN oauth_credentials oc ON oc.installation_id = i.id
WHERE i.account_id = $1
  AND i.status = 'active'
  AND ig.status = 'active'
  AND COALESCE(i.settings->>'origin', '') <> 'fixture'
ORDER BY COALESCE(oc.refreshed_at, oc.created_at) DESC, i.updated_at DESC, i.id DESC
LIMIT 1`, accountID).Scan(&connection.InstallationID, &connection.IntegrationID, &connection.IntegrationCode)
	if err == nil {
		return connection, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, fmt.Errorf("resolve reading connection: %w", err)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM installations WHERE account_id = $1)`, accountID).Scan(&exists); err != nil {
		return Connection{}, fmt.Errorf("check account installations: %w", err)
	}
	if !exists {
		return Connection{}, ErrAccountNotFound
	}
	return Connection{}, ErrNoAuthorizedConnection
}
