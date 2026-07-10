package oauth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
)

type Cipher interface {
	Seal(plaintext, additionalData []byte) ([]byte, int, error)
	Open(keyVersion int, ciphertext, additionalData []byte) ([]byte, error)
}

type Integration struct {
	ID                     uuid.UUID
	Code                   string
	ClientID               string
	ClientSecretCiphertext []byte
	ClientSecretKeyVersion int
	RedirectURI            string
	WebhookEvents          []string
}

type State struct {
	ID uuid.UUID
	Integration
	ReturnURL string
	ExpiresAt time.Time
}

type InstallationResult struct {
	ID        uuid.UUID `json:"installation_id"`
	AccountID int64     `json:"account_id"`
	Status    string    `json:"status"`
}

type OAuthGateway interface {
	ExchangeCode(context.Context, string, string, string, string, string) (Token, error)
	Refresh(context.Context, string, string, string, string, string) (Token, error)
	GetAccount(context.Context, string, string) (Account, error)
}

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	ServerTime   int64
}

type Account struct {
	ID        int64
	Subdomain string
}

func integrationSecretAAD(id uuid.UUID) []byte {
	return cryptox.IntegrationSecretAAD(id)
}

func credentialsAAD(id uuid.UUID) []byte {
	return cryptox.InstallationOAuthAAD(id)
}

func webhookKeyAAD(id uuid.UUID) []byte {
	return cryptox.InstallationWebhookKeyAAD(id)
}
