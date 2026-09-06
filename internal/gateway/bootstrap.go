package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"strings"
	"time"
)

type BootstrapLookup interface {
	KnownAccount(context.Context, uuid.UUID, string) (int64, error)
}
type BootstrapAPI interface {
	BootstrapAccount(context.Context, uuid.UUID, int64, string, string) (amocrm.Account, error)
}
type BootstrapService struct {
	lookup BootstrapLookup
	api    BootstrapAPI
}

func NewBootstrap(pool *pgxpool.Pool, api *amocrm.Client) *BootstrapService {
	return NewBootstrapWithLookup(bootstrapDatabase{pool}, api)
}
func NewBootstrapWithLookup(lookup BootstrapLookup, api BootstrapAPI) *BootstrapService {
	return &BootstrapService{lookup: lookup, api: api}
}

func (s *BootstrapService) GetAccount(ctx context.Context, r serviceapi.BootstrapAccountRequest) (serviceapi.BootstrapAccount, error) {
	if corepolicy.Caller(ctx) != serviceapi.CoreService {
		return serviceapi.BootstrapAccount{}, serviceapi.Fail(serviceapi.PermissionDenied, "bootstrap account is restricted to Core")
	}
	if r.IntegrationID == uuid.Nil || len(r.AccountDomain) > 253 || len(r.AccessToken) == 0 || len(r.AccessToken) > 16384 || strings.ContainsAny(r.AccessToken, "\r\n") {
		return serviceapi.BootstrapAccount{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid bootstrap account request")
	}
	base, err := amocrm.AccountBaseURL(r.AccountDomain)
	if err != nil {
		return serviceapi.BootstrapAccount{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid amoCRM account domain")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	known, err := s.lookup.KnownAccount(ctx, r.IntegrationID, base.Host)
	if err != nil {
		return serviceapi.BootstrapAccount{}, err
	}
	account, err := s.api.BootstrapAccount(ctx, r.IntegrationID, known, base.Host, r.AccessToken)
	if err != nil {
		return serviceapi.BootstrapAccount{}, corepolicy.MapUpstreamError(err)
	}
	return serviceapi.BootstrapAccount{ID: account.ID, Subdomain: account.Subdomain}, nil
}

type bootstrapDatabase struct{ pool *pgxpool.Pool }

func (d bootstrapDatabase) KnownAccount(ctx context.Context, integration uuid.UUID, domain string) (int64, error) {
	var active bool
	var first, last int64
	err := d.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM integrations WHERE id=$1 AND status='active'),coalesce(min(account_id),0),coalesce(max(account_id),0) FROM installations WHERE lower(rtrim(account_domain,'.'))=$2`, integration, domain).Scan(&active, &first, &last)
	if err != nil {
		return 0, serviceapi.Fail(serviceapi.Unavailable, "bootstrap policy is unavailable")
	}
	if !active {
		return 0, serviceapi.Fail(serviceapi.PermissionDenied, "integration is not active")
	}
	if first != last {
		return 0, serviceapi.Fail(serviceapi.Conflict, "account domain has conflicting identities")
	}
	return first, nil
}
