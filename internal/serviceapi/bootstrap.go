package serviceapi

import (
	"context"
	"github.com/google/uuid"
)

// BootstrapAccountRequest is Core-to-Core OAuth lifecycle traffic. The newly
// exchanged access token is transient: never store it in a service cache/log.
// Product services do not receive this capability or possess these credentials.
type BootstrapAccountRequest struct {
	IntegrationID uuid.UUID `json:"integration_id"`
	AccountDomain string    `json:"account_domain"`
	AccessToken   string    `json:"access_token"`
}
type BootstrapAccount struct {
	ID        int64  `json:"id"`
	Subdomain string `json:"subdomain"`
}
type CoreBootstrap interface {
	GetAccount(context.Context, BootstrapAccountRequest) (BootstrapAccount, error)
}
