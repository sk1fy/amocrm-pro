package widgetauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
)

const maxTokenLength = 16 * 1024

var integerJSON = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

type tokenHint struct {
	ClientUUID string
	AccountID  int64
}

type unverifiedClaims struct {
	jwt.RegisteredClaims
	AccountID  int64  `json:"account_id"`
	ClientUUID string `json:"client_uuid"`
}

// Authenticator validates amoCRM disposable JWTs and consumes their jti in the
// durable repository before returning a principal.
type Authenticator struct {
	repository Repository
	secrets    SecretOpener
	clock      func() time.Time
	leeway     time.Duration
}

// Option configures an Authenticator.
type Option func(*Authenticator) error

// WithClock overrides time.Now. It is primarily useful for deterministic tests.
func WithClock(clock func() time.Time) Option {
	return func(authenticator *Authenticator) error {
		if clock == nil {
			return fmt.Errorf("widget auth clock is nil")
		}
		authenticator.clock = clock
		return nil
	}
}

// WithLeeway allows a non-negative amount of clock skew for iat, nbf, and exp.
// The default is zero for strict validation.
func WithLeeway(leeway time.Duration) Option {
	return func(authenticator *Authenticator) error {
		if leeway < 0 {
			return fmt.Errorf("widget auth leeway must not be negative")
		}
		authenticator.leeway = leeway
		return nil
	}
}

func NewAuthenticator(repository Repository, secrets SecretOpener, options ...Option) (*Authenticator, error) {
	if repository == nil {
		return nil, fmt.Errorf("widget auth repository is nil")
	}
	if secrets == nil {
		return nil, fmt.Errorf("widget auth secret opener is nil")
	}

	authenticator := &Authenticator{
		repository: repository,
		secrets:    secrets,
		clock:      time.Now,
	}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("widget auth option is nil")
		}
		if err := option(authenticator); err != nil {
			return nil, err
		}
	}
	return authenticator, nil
}

// Verify validates rawToken without consuming its jti. Callers must either use
// Authenticate or durably consume Principal.UsedToken in the same transaction
// as the operation authorized by the token.
func (a *Authenticator) Verify(ctx context.Context, rawToken string) (Principal, error) {
	if a == nil {
		return Principal{}, fmt.Errorf("widget authenticator is nil")
	}

	hint, err := parseUnverifiedHint(rawToken)
	if err != nil {
		return Principal{}, err
	}
	material, err := a.repository.FindVerificationMaterial(ctx, hint.ClientUUID, hint.AccountID)
	if errors.Is(err, ErrUnknownTenant) {
		return Principal{}, fmt.Errorf("%w: tenant lookup failed", ErrInvalidToken)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("lookup widget verification material: %w", err)
	}
	if err := validateMaterial(material, hint); err != nil {
		return Principal{}, fmt.Errorf("invalid widget verification material: %w", err)
	}

	expectedAudience, err := AudienceForRedirectURI(material.RedirectURI)
	if err != nil {
		return Principal{}, fmt.Errorf("derive widget audience: %w", err)
	}
	expectedIssuer, err := IssuerForAccountDomain(material.AccountDomain)
	if err != nil {
		return Principal{}, fmt.Errorf("derive widget issuer: %w", err)
	}

	secret, err := a.secrets.Open(
		material.ClientSecretKeyVersion,
		material.ClientSecretCiphertext,
		cryptox.IntegrationSecretAAD(material.IntegrationID),
	)
	if err != nil {
		return Principal{}, fmt.Errorf("decrypt widget signing secret: %w", err)
	}
	defer clear(secret)
	if len(secret) == 0 {
		return Principal{}, fmt.Errorf("decrypted widget signing secret is empty")
	}

	now := a.clock().UTC()
	claims, err := a.parseVerified(rawToken, secret, now)
	if err != nil {
		return Principal{}, err
	}
	if err := validateStrictPayloadShape(rawToken); err != nil {
		return Principal{}, fmt.Errorf("%w: payload shape: %v", ErrInvalidToken, err)
	}
	if err := validateClaims(claims, material, expectedAudience, expectedIssuer, now, a.leeway); err != nil {
		return Principal{}, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}

	return Principal{
		IntegrationID:    material.IntegrationID,
		InstallationID:   material.InstallationID,
		AccountID:        claims.AccountID,
		UserID:           claims.UserID,
		ClientUUID:       claims.ClientUUID,
		Issuer:           expectedIssuer,
		TokenID:          claims.ID,
		TokenRetainUntil: claims.ExpiresAt.Time.UTC().Add(a.leeway),
	}, nil
}

// Authenticate validates rawToken and atomically consumes its jti. A principal
// is never returned for a replayed token.
func (a *Authenticator) Authenticate(ctx context.Context, rawToken string) (Principal, error) {
	principal, err := a.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, err
	}
	if err := a.repository.ConsumeToken(ctx, principal.UsedToken()); err != nil {
		if errors.Is(err, ErrReplay) {
			return Principal{}, ErrReplay
		}
		return Principal{}, fmt.Errorf("persist widget token consumption: %w", err)
	}
	return principal, nil
}

func parseUnverifiedHint(rawToken string) (tokenHint, error) {
	if rawToken == "" || len(rawToken) > maxTokenLength || rawToken != strings.TrimSpace(rawToken) {
		return tokenHint{}, fmt.Errorf("%w: token is empty, too large, or has surrounding whitespace", ErrInvalidToken)
	}

	unverified := &unverifiedClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	token, _, err := parser.ParseUnverified(rawToken, unverified)
	if err != nil || token == nil || token.Method == nil {
		return tokenHint{}, fmt.Errorf("%w: malformed compact token", ErrInvalidToken)
	}
	if token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
		return tokenHint{}, fmt.Errorf("%w: signing algorithm must be HS256", ErrInvalidToken)
	}
	if !isSafeIdentifier(unverified.ClientUUID) || unverified.AccountID <= 0 {
		return tokenHint{}, fmt.Errorf("%w: invalid client_uuid or account_id hint", ErrInvalidToken)
	}
	return tokenHint{ClientUUID: unverified.ClientUUID, AccountID: unverified.AccountID}, nil
}

func (a *Authenticator) parseVerified(rawToken string, secret []byte, now time.Time) (*Claims, error) {
	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(a.leeway),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(rawToken, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing algorithm")
		}
		return secret, nil
	})
	if err != nil || token == nil || !token.Valid {
		return nil, fmt.Errorf("%w: signature or registered claims validation failed", ErrInvalidToken)
	}
	return claims, nil
}

func validateMaterial(material VerificationMaterial, hint tokenHint) error {
	if material.IntegrationID == uuid.Nil || material.InstallationID == uuid.Nil {
		return fmt.Errorf("integration and installation IDs are required")
	}
	if material.ClientUUID != hint.ClientUUID || material.AccountID != hint.AccountID {
		return fmt.Errorf("lookup result does not match token hint")
	}
	if !isSafeIdentifier(material.ClientUUID) {
		return fmt.Errorf("client UUID is invalid")
	}
	if len(material.ClientSecretCiphertext) == 0 || material.ClientSecretKeyVersion <= 0 {
		return fmt.Errorf("encrypted client secret and positive key version are required")
	}
	if material.RedirectURI == "" || material.AccountDomain == "" {
		return fmt.Errorf("redirect URI and account domain are required")
	}
	return nil
}

func validateClaims(
	claims *Claims,
	material VerificationMaterial,
	expectedAudience string,
	expectedIssuer string,
	now time.Time,
	leeway time.Duration,
) error {
	if claims == nil || claims.ExpiresAt == nil || claims.IssuedAt == nil || claims.NotBefore == nil {
		return fmt.Errorf("iat, nbf, and exp are required")
	}
	if !isSafeIdentifier(claims.ID) {
		return fmt.Errorf("jti is invalid")
	}
	if claims.ClientUUID != material.ClientUUID || !isSafeIdentifier(claims.ClientUUID) {
		return fmt.Errorf("client_uuid mismatch")
	}
	if claims.AccountID != material.AccountID || claims.AccountID <= 0 || claims.UserID <= 0 {
		return fmt.Errorf("account_id or user_id is invalid")
	}
	if len(claims.Audience) != 1 {
		return fmt.Errorf("aud must contain exactly one origin")
	}
	audience, err := normalizeClaimOrigin(claims.Audience[0], false)
	if err != nil || audience != expectedAudience {
		return fmt.Errorf("aud mismatch")
	}
	issuer, err := normalizeClaimOrigin(claims.Issuer, true)
	if err != nil || issuer != expectedIssuer {
		return fmt.Errorf("iss mismatch")
	}

	if !claims.ExpiresAt.Time.After(claims.IssuedAt.Time) || !claims.ExpiresAt.Time.After(claims.NotBefore.Time) {
		return fmt.Errorf("exp must be after iat and nbf")
	}
	if !now.Before(claims.ExpiresAt.Time.Add(leeway)) {
		return fmt.Errorf("token is expired")
	}
	if now.Add(leeway).Before(claims.NotBefore.Time) {
		return fmt.Errorf("token is not active yet")
	}
	if now.Add(leeway).Before(claims.IssuedAt.Time) {
		return fmt.Errorf("token was issued in the future")
	}
	return nil
}

func isSafeIdentifier(value string) bool {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

// validateStrictPayloadShape rejects lossy JSON representations (for example
// fractional NumericDate values or an aud array) while still allowing future
// amoCRM claims such as subdomain.
func validateStrictPayloadShape(rawToken string) error {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 || parts[1] == "" {
		return fmt.Errorf("compact token must have three non-empty segments")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("payload is not strict unpadded base64url")
	}

	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return fmt.Errorf("payload must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read claim name")
		}
		name, ok := nameToken.(string)
		if !ok {
			return fmt.Errorf("claim name must be a string")
		}
		if _, duplicate := fields[name]; duplicate {
			return fmt.Errorf("claim %q is duplicated", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode claim %q", name)
		}
		fields[name] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return fmt.Errorf("payload object is not terminated")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("payload has trailing JSON data")
	}

	for _, name := range []string{"iss", "aud", "jti", "client_uuid"} {
		raw, exists := fields[name]
		if !exists {
			return fmt.Errorf("claim %q is required", name)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			return fmt.Errorf("claim %q must be a non-empty string", name)
		}
	}
	for _, name := range []string{"iat", "nbf", "exp", "account_id", "user_id"} {
		raw, exists := fields[name]
		if !exists {
			return fmt.Errorf("claim %q is required", name)
		}
		if !integerJSON.Match(raw) {
			return fmt.Errorf("claim %q must be a JSON integer", name)
		}
	}
	return nil
}
