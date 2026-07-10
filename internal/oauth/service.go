package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Service struct {
	store        *Store
	cipher       Cipher
	gateway      OAuthGateway
	stateTTL     time.Duration
	requestTTL   time.Duration
	authorizeURL string
}

func NewService(store *Store, cipher Cipher, gateway OAuthGateway, stateTTL, requestTTL time.Duration) *Service {
	return &Service{
		store: store, cipher: cipher, gateway: gateway,
		stateTTL: stateTTL, requestTTL: requestTTL,
		authorizeURL: "https://www.amocrm.ru/oauth",
	}
}

func (s *Service) Start(ctx context.Context, integrationCode, returnURL string) (string, error) {
	integration, err := s.store.FindIntegrationByCode(ctx, strings.TrimSpace(integrationCode))
	if err != nil {
		return "", err
	}
	if err := validateReturnURL(returnURL); err != nil {
		return "", err
	}
	state, _, err := s.store.CreateState(ctx, integration.ID, returnURL, s.stateTTL)
	if err != nil {
		return "", err
	}
	authorize, err := url.Parse(s.authorizeURL)
	if err != nil {
		return "", err
	}
	query := authorize.Query()
	query.Set("client_id", integration.ClientID)
	query.Set("state", state)
	query.Set("mode", "popup")
	authorize.RawQuery = query.Encode()
	return authorize.String(), nil
}

func (s *Service) Callback(ctx context.Context, rawState, code, referer string) (InstallationResult, error) {
	state, err := s.store.ConsumeState(ctx, strings.TrimSpace(rawState))
	if err != nil {
		return InstallationResult{}, err
	}
	if strings.TrimSpace(code) == "" {
		return InstallationResult{}, errors.New("authorization code is required")
	}
	accountURL, err := amocrm.AccountBaseURL(referer)
	if err != nil {
		return InstallationResult{}, fmt.Errorf("invalid account referer: %w", err)
	}
	clientSecret, err := s.cipher.Open(
		state.ClientSecretKeyVersion,
		state.ClientSecretCiphertext,
		integrationSecretAAD(state.Integration.ID),
	)
	if err != nil {
		return InstallationResult{}, fmt.Errorf("decrypt integration secret: %w", err)
	}

	requestContext, cancel := context.WithTimeout(ctx, s.requestTTL)
	token, err := s.gateway.ExchangeCode(
		requestContext,
		accountURL.Host,
		state.ClientID,
		string(clientSecret),
		state.RedirectURI,
		code,
	)
	if err == nil {
		var account Account
		account, err = s.gateway.GetAccount(requestContext, accountURL.Host, token.AccessToken)
		if err == nil {
			if account.ID <= 0 {
				err = errors.New("amoCRM returned invalid account id")
			} else {
				result, saveErr := s.store.SaveInstallation(ctx, state.Integration, account, accountURL.Host, token)
				cancel()
				return result, saveErr
			}
		}
	}
	cancel()
	return InstallationResult{}, err
}

func validateReturnURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return errors.New("return_url must be an absolute-path reference on this service")
	}
	return nil
}
