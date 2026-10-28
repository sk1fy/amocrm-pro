package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

// amoCRM authorization codes live 20 minutes; a claimed code is remembered as long.
const installCodeTTL = 20 * time.Minute

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
	return s.authorize(ctx, state.Integration, accountURL.Host, code)
}

// InstallCallback completes the authorization amoCRM sends by itself when a
// widget archive is installed from amoMarket. That request has client_id and
// from_widget but never state, so the integration comes from its active
// client_id and every code is claimed once before the exchange. Only the
// client secret can redeem the code; the account still comes from the token.
func (s *Service) InstallCallback(ctx context.Context, rawClientID, code, referer string) (InstallationResult, error) {
	clientID := strings.TrimSpace(rawClientID)
	if _, err := uuid.Parse(clientID); err != nil || strings.TrimSpace(code) == "" {
		return InstallationResult{}, ErrInvalidState
	}
	accountURL, err := amocrm.AccountBaseURL(referer)
	if err != nil {
		return InstallationResult{}, fmt.Errorf("%w: invalid account referer", ErrInvalidState)
	}
	integration, err := s.store.FindIntegrationByClientID(ctx, clientID)
	if errors.Is(err, ErrIntegrationNotFound) {
		return InstallationResult{}, ErrInvalidState
	}
	if err != nil {
		return InstallationResult{}, err
	}
	if err := s.store.ClaimInstallCode(ctx, integration.ID, code, installCodeTTL); err != nil {
		return InstallationResult{}, err
	}
	return s.authorize(ctx, integration, accountURL.Host, code)
}

func (s *Service) authorize(ctx context.Context, integration Integration, accountDomain, code string) (InstallationResult, error) {
	clientSecret, err := s.cipher.Open(
		integration.ClientSecretKeyVersion,
		integration.ClientSecretCiphertext,
		integrationSecretAAD(integration.ID),
	)
	if err != nil {
		return InstallationResult{}, fmt.Errorf("decrypt integration secret: %w", err)
	}

	requestContext, cancel := context.WithTimeout(ctx, s.requestTTL)
	token, err := s.gateway.ExchangeCode(
		requestContext,
		accountDomain,
		integration.ClientID,
		string(clientSecret),
		integration.RedirectURI,
		code,
	)
	if err == nil {
		var account Account
		// The budget identity comes from the verified state or claimed install
		// code, not callback parameters. This keeps the OAuth port compatible.
		accountContext := context.WithValue(requestContext, accountBudgetContextKey{}, integration.ID)
		account, err = s.gateway.GetAccount(accountContext, accountDomain, token.AccessToken)
		if err == nil {
			if account.ID <= 0 {
				err = errors.New("amoCRM returned invalid account id")
			} else {
				result, saveErr := s.store.SaveInstallation(ctx, integration, account, accountDomain, token)
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
