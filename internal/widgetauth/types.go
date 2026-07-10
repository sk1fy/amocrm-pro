// Package widgetauth authenticates disposable JWTs issued by amoCRM for
// requests made by a JS widget.
package widgetauth

import (
	"context"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	// ErrInvalidToken marks malformed, expired, incorrectly signed, or otherwise
	// untrusted widget tokens. Callers must not expose the wrapped detail.
	ErrInvalidToken = errors.New("invalid widget token")
	// ErrReplay marks a jti that has already been consumed for an integration.
	ErrReplay = errors.New("widget token was already used")
	// ErrUnknownTenant is returned by repositories when the unverified lookup
	// hint does not identify an active integration installation.
	ErrUnknownTenant = errors.New("widget tenant not found")
)

// Claims is the documented amoCRM disposable-token payload.
type Claims struct {
	jwt.RegisteredClaims
	AccountID  int64  `json:"account_id"`
	UserID     int64  `json:"user_id"`
	ClientUUID string `json:"client_uuid"`
}

// VerificationMaterial contains the trusted database values needed to verify
// a token. It is selected using the unverified client_uuid/account_id hint.
type VerificationMaterial struct {
	IntegrationID          uuid.UUID
	InstallationID         uuid.UUID
	ClientUUID             string
	ClientSecretCiphertext []byte
	ClientSecretKeyVersion int
	RedirectURI            string
	AccountID              int64
	AccountDomain          string
}

// UsedToken is persisted atomically after all JWT validation succeeds.
type UsedToken struct {
	IntegrationID uuid.UUID
	TokenID       string
	Issuer        string
	AccountID     int64
	UserID        int64
	ExpiresAt     time.Time
}

// Repository resolves verification material and provides durable single-use
// token consumption.
type Repository interface {
	FindVerificationMaterial(ctx context.Context, clientUUID string, accountID int64) (VerificationMaterial, error)
	ConsumeToken(ctx context.Context, token UsedToken) error
}

// SecretOpener is implemented by cryptox.KeyRing.
type SecretOpener interface {
	Open(keyVersion int, ciphertext, additionalData []byte) ([]byte, error)
}

// Principal is safe to place in a request context only after Authenticate has
// completed, including durable replay protection.
type Principal struct {
	IntegrationID  uuid.UUID
	InstallationID uuid.UUID
	AccountID      int64
	UserID         int64
	ClientUUID     string
	Issuer         string
	TokenID        string
}

type principalContextKey struct{}

// ContextWithPrincipal returns a child context containing a verified widget
// principal.
func ContextWithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFromContext returns the verified widget principal, when present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}
