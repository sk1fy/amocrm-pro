package oauth

import (
	"context"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Gateway struct {
	client *amocrm.OAuthClient
}

func NewGateway(client *amocrm.OAuthClient) *Gateway { return &Gateway{client: client} }

func (g *Gateway) ExchangeCode(
	ctx context.Context,
	accountDomain, clientID, clientSecret, redirectURI, code string,
) (Token, error) {
	response, err := g.client.ExchangeCode(ctx, accountDomain, amocrm.OAuthCredentials{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirectURI,
	}, code)
	if err != nil {
		return Token{}, err
	}
	return Token{
		AccessToken: response.AccessToken, RefreshToken: response.RefreshToken,
		ExpiresIn: response.ExpiresIn, ServerTime: response.ServerTime,
	}, nil
}

func (g *Gateway) Refresh(
	ctx context.Context,
	accountDomain, clientID, clientSecret, redirectURI, refreshToken string,
) (Token, error) {
	response, err := g.client.Refresh(ctx, accountDomain, amocrm.OAuthCredentials{
		ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirectURI,
	}, refreshToken)
	if err != nil {
		return Token{}, err
	}
	return Token{
		AccessToken: response.AccessToken, RefreshToken: response.RefreshToken,
		ExpiresIn: response.ExpiresIn, ServerTime: response.ServerTime,
	}, nil
}

func (g *Gateway) GetAccount(ctx context.Context, accountDomain, accessToken string) (Account, error) {
	response, err := g.client.GetAccount(ctx, accountDomain, accessToken)
	if err != nil {
		return Account{}, err
	}
	return Account{ID: response.ID, Subdomain: response.Subdomain}, nil
}
