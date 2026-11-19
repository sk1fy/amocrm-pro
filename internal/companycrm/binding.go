package companycrm

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

var ErrBinding = errors.New("company CRM binding is unavailable")

type Resolver interface {
	Resolve(context.Context) (Binding, error)
}

type Store struct {
	Pool            *pgxpool.Pool
	Binding         Binding
	IntegrationCode string
}

func (s *Store) Resolve(ctx context.Context) (Binding, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b := s.Binding
	var valid bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM installations i
 JOIN integrations g ON g.id=i.integration_id
 JOIN oauth_credentials c ON c.installation_id=i.id
 WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3
 AND i.status='active' AND g.status='active' AND ($4='' OR g.code=$4)
 AND COALESCE(i.settings->>'origin','') <> 'fixture'
 )`, b.InstallationID, b.IntegrationID, b.AccountID, s.IntegrationCode).Scan(&valid)
	if err != nil {
		return Binding{}, err
	}
	if !valid {
		return Binding{}, ErrBinding
	}
	return b, nil
}

// ScopedTokens validates every credential lookup, including a 401 refresh.
type ScopedTokens struct {
	Provider amocrm.TokenProvider
	Binding  Binding
	Host     string
}

func (s ScopedTokens) check(t amocrm.AccessToken, err error) (amocrm.AccessToken, error) {
	if err != nil {
		return amocrm.AccessToken{}, err
	}
	if t.InstallationID != s.Binding.InstallationID || t.IntegrationID != s.Binding.IntegrationID || t.AccountID != s.Binding.AccountID {
		return amocrm.AccessToken{}, ErrBinding
	}
	if s.Host != "" {
		base, err := amocrm.AccountBaseURL(t.AccountDomain)
		if err != nil || base.Hostname() != s.Host {
			return amocrm.AccessToken{}, ErrBinding
		}
	}
	return t, nil
}
func (s ScopedTokens) Token(ctx context.Context, id uuid.UUID) (amocrm.AccessToken, error) {
	if id != s.Binding.InstallationID {
		return amocrm.AccessToken{}, ErrBinding
	}
	return s.check(s.Provider.Token(ctx, id))
}
func (s ScopedTokens) RefreshIfCurrent(ctx context.Context, t amocrm.AccessToken) (amocrm.AccessToken, error) {
	if _, err := s.check(t, nil); err != nil {
		return amocrm.AccessToken{}, err
	}
	return s.check(s.Provider.RefreshIfCurrent(ctx, t))
}
func (s ScopedTokens) MarkReauthRequired(ctx context.Context, id uuid.UUID, v int64) error {
	if id != s.Binding.InstallationID {
		return ErrBinding
	}
	return s.Provider.MarkReauthRequired(ctx, id, v)
}
