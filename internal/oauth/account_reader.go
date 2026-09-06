package oauth

import (
	"context"
	"errors"
	"github.com/google/uuid"
)

// AccountReader routes the API v4 lookup through the shared Core-owned Gateway.
// Exchange/refresh remain on the existing OAuthGateway. The transient access
// token stays within Core and is neither stored here nor exposed to products.
type AccountReader func(context.Context, uuid.UUID, string, string) (Account, error)
type accountBudgetContextKey struct{}
type accountReaderGateway struct {
	OAuthGateway
	reader AccountReader
}

func WithAccountReader(base OAuthGateway, reader AccountReader) OAuthGateway {
	return accountReaderGateway{OAuthGateway: base, reader: reader}
}
func (g accountReaderGateway) GetAccount(ctx context.Context, domain, accessToken string) (Account, error) {
	id, ok := ctx.Value(accountBudgetContextKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil || g.reader == nil {
		return Account{}, errors.New("trusted OAuth integration context is required for account lookup")
	}
	return g.reader(ctx, id, domain, accessToken)
}
