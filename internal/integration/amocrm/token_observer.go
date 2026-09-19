package amocrm

import (
	"context"
	"github.com/google/uuid"
)

// WithCredentialVersionObserver retains the shared refresh provider and its
// recovery state. Only the version is observed; credential values never leave
// the API client. The callback belongs to one synchronous request.
func (c *Client) WithCredentialVersionObserver(observe func(int64)) *Client {
	return c.WithTokenProvider(&observedProvider{TokenProvider: c.tokens, observe: observe})
}

type observedProvider struct {
	TokenProvider
	observe func(int64)
}

func (p *observedProvider) Token(ctx context.Context, id uuid.UUID) (AccessToken, error) {
	v, e := p.TokenProvider.Token(ctx, id)
	if e == nil {
		p.observe(v.TokenVersion)
	}
	return v, e
}
func (p *observedProvider) RefreshIfCurrent(ctx context.Context, old AccessToken) (AccessToken, error) {
	v, e := p.TokenProvider.RefreshIfCurrent(ctx, old)
	if e == nil {
		p.observe(v.TokenVersion)
	}
	return v, e
}
