package distribution

import (
	"context"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

// referenceAccountDomain uses the installation already verified against the
// account API. Only a canonical, allowlisted account host crosses the bridge.
func (s *Store) referenceAccountDomain(ctx context.Context, b Binding) (string, error) {
	var domain string
	if err := s.pool.QueryRow(ctx, `SELECT account_domain FROM installations WHERE id=$1 AND account_id=$2 AND integration_id=$3 AND status='active'`, b.InstallationID, b.AccountID, b.IntegrationID).Scan(&domain); err != nil {
		return "", err
	}
	u, err := amocrm.AccountBaseURL(domain)
	if err != nil {
		return "", ErrUnavailable
	}
	return u.Host, nil
}
