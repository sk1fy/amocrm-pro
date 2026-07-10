package widgetauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
)

const (
	testClientUUID = "0b0832f6-d123-4123-9123-e73f236833c"
	testTokenID    = "d628f123-5123-473e-a123-ed123ef31f8f"
	testSecret     = "a-client-secret-long-enough-for-hs256"
)

var testNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

type fakeRepository struct {
	material      VerificationMaterial
	findError     error
	consumeError  error
	lookupCalls   int
	consumeCalls  int
	lookupClient  string
	lookupAccount int64
	consumed      []UsedToken
	seen          map[string]struct{}
}

func (repository *fakeRepository) FindVerificationMaterial(
	_ context.Context,
	clientUUID string,
	accountID int64,
) (VerificationMaterial, error) {
	repository.lookupCalls++
	repository.lookupClient = clientUUID
	repository.lookupAccount = accountID
	if repository.findError != nil {
		return VerificationMaterial{}, repository.findError
	}
	if clientUUID != repository.material.ClientUUID || accountID != repository.material.AccountID {
		return VerificationMaterial{}, ErrUnknownTenant
	}
	return repository.material, nil
}

func (repository *fakeRepository) ConsumeToken(_ context.Context, token UsedToken) error {
	repository.consumeCalls++
	if repository.consumeError != nil {
		return repository.consumeError
	}
	if repository.seen == nil {
		repository.seen = make(map[string]struct{})
	}
	key := token.IntegrationID.String() + ":" + token.TokenID
	if _, exists := repository.seen[key]; exists {
		return ErrReplay
	}
	repository.seen[key] = struct{}{}
	repository.consumed = append(repository.consumed, token)
	return nil
}

type authFixture struct {
	authenticator *Authenticator
	repository    *fakeRepository
	claims        jwt.MapClaims
	secret        []byte
}

func newAuthFixture(t *testing.T) authFixture {
	t.Helper()

	integrationID := uuid.MustParse("38f1842b-082a-4ea5-a88e-9982706b85ad")
	installationID := uuid.MustParse("01dc62ae-105a-4b6b-9cf4-f54bca9a00a7")
	ring, err := cryptox.NewKeyRing(
		map[int][]byte{3: bytes.Repeat([]byte{0x7a}, cryptox.KeySize)},
		3,
	)
	if err != nil {
		t.Fatalf("cryptox.NewKeyRing() error = %v", err)
	}
	secret := []byte(testSecret)
	ciphertext, keyVersion, err := ring.Seal(secret, cryptox.IntegrationSecretAAD(integrationID))
	if err != nil {
		t.Fatalf("KeyRing.Seal() error = %v", err)
	}

	repository := &fakeRepository{material: VerificationMaterial{
		IntegrationID:          integrationID,
		InstallationID:         installationID,
		ClientUUID:             testClientUUID,
		ClientSecretCiphertext: ciphertext,
		ClientSecretKeyVersion: keyVersion,
		RedirectURI:            "https://External.Integration.io/widget/callback?source=amo",
		AccountID:              12345678,
		AccountDomain:          "Subdomain.AmoCRM.ru",
	}}
	authenticator, err := NewAuthenticator(repository, ring, WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}

	claims := jwt.MapClaims{
		"iss":         "https://subdomain.amocrm.ru",
		"aud":         "https://external.integration.io",
		"jti":         testTokenID,
		"iat":         testNow.Add(-time.Minute).Unix(),
		"nbf":         testNow.Add(-time.Minute).Unix(),
		"exp":         testNow.Add(10 * time.Minute).Unix(),
		"account_id":  int64(12345678),
		"user_id":     int64(87654321),
		"client_uuid": testClientUUID,
		"subdomain":   "subdomain",
	}
	return authFixture{
		authenticator: authenticator,
		repository:    repository,
		claims:        claims,
		secret:        secret,
	}
}

func signClaims(t *testing.T, claims jwt.MapClaims, secret []byte, method jwt.SigningMethod) string {
	t.Helper()
	rawToken, err := jwt.NewWithClaims(method, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("SignedString() error = %v", err)
	}
	return rawToken
}

func cloneClaims(source jwt.MapClaims) jwt.MapClaims {
	result := make(jwt.MapClaims, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func TestAuthenticateReturnsVerifiedPrincipalAndConsumesToken(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
	principal, err := fixture.authenticator.Authenticate(context.Background(), rawToken)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	if principal.IntegrationID != fixture.repository.material.IntegrationID ||
		principal.InstallationID != fixture.repository.material.InstallationID ||
		principal.AccountID != 12345678 ||
		principal.UserID != 87654321 ||
		principal.ClientUUID != testClientUUID ||
		principal.Issuer != "https://subdomain.amocrm.ru" ||
		principal.TokenID != testTokenID {
		t.Fatalf("Authenticate() principal = %+v", principal)
	}
	if fixture.repository.lookupCalls != 1 || fixture.repository.consumeCalls != 1 {
		t.Fatalf("repository calls = lookup %d, consume %d, want 1/1", fixture.repository.lookupCalls, fixture.repository.consumeCalls)
	}
	if fixture.repository.lookupClient != testClientUUID || fixture.repository.lookupAccount != 12345678 {
		t.Fatalf("lookup hint = %q/%d", fixture.repository.lookupClient, fixture.repository.lookupAccount)
	}
	if len(fixture.repository.consumed) != 1 {
		t.Fatalf("consumed token count = %d, want 1", len(fixture.repository.consumed))
	}
	used := fixture.repository.consumed[0]
	if used.TokenID != testTokenID || used.Issuer != "https://subdomain.amocrm.ru" ||
		!used.ExpiresAt.Equal(testNow.Add(10*time.Minute)) {
		t.Fatalf("consumed token = %+v", used)
	}
}

func TestAuthenticateRejectsReplay(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
	if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); err != nil {
		t.Fatalf("first Authenticate() error = %v", err)
	}
	if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); !errors.Is(err, ErrReplay) {
		t.Fatalf("second Authenticate() error = %v, want ErrReplay", err)
	}
	if fixture.repository.consumeCalls != 2 || len(fixture.repository.consumed) != 1 {
		t.Fatalf("consume calls/rows = %d/%d, want 2/1", fixture.repository.consumeCalls, len(fixture.repository.consumed))
	}
}

func TestAuthenticateRejectsBeforeLookupForInvalidAlgorithmOrHint(t *testing.T) {
	t.Parallel()

	tests := map[string]func(jwt.MapClaims) (jwt.MapClaims, jwt.SigningMethod){
		"HS384": func(claims jwt.MapClaims) (jwt.MapClaims, jwt.SigningMethod) {
			return claims, jwt.SigningMethodHS384
		},
		"invalid client UUID": func(claims jwt.MapClaims) (jwt.MapClaims, jwt.SigningMethod) {
			claims["client_uuid"] = " client-id "
			return claims, jwt.SigningMethodHS256
		},
		"zero account": func(claims jwt.MapClaims) (jwt.MapClaims, jwt.SigningMethod) {
			claims["account_id"] = int64(0)
			return claims, jwt.SigningMethodHS256
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t)
			claims, method := mutate(cloneClaims(fixture.claims))
			rawToken := signClaims(t, claims, fixture.secret, method)
			if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Authenticate() error = %v, want ErrInvalidToken", err)
			}
			if fixture.repository.lookupCalls != 0 || fixture.repository.consumeCalls != 0 {
				t.Fatalf("repository calls = lookup %d, consume %d, want 0/0", fixture.repository.lookupCalls, fixture.repository.consumeCalls)
			}
		})
	}
}

func TestAuthenticateUsesUnverifiedValuesOnlyAsLookupHint(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	rawToken := signClaims(t, fixture.claims, []byte("wrong signing secret"), jwt.SigningMethodHS256)
	if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidToken", err)
	}
	if fixture.repository.lookupCalls != 1 {
		t.Fatalf("lookup calls = %d, want 1", fixture.repository.lookupCalls)
	}
	if fixture.repository.consumeCalls != 0 {
		t.Fatalf("consume calls = %d, want 0", fixture.repository.consumeCalls)
	}
}

func TestAuthenticateStrictClaimsValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]func(jwt.MapClaims){
		"missing issuer":       func(claims jwt.MapClaims) { delete(claims, "iss") },
		"issuer mismatch":      func(claims jwt.MapClaims) { claims["iss"] = "https://other.amocrm.ru" },
		"issuer has path":      func(claims jwt.MapClaims) { claims["iss"] = "https://subdomain.amocrm.ru/path" },
		"missing audience":     func(claims jwt.MapClaims) { delete(claims, "aud") },
		"audience mismatch":    func(claims jwt.MapClaims) { claims["aud"] = "https://attacker.example" },
		"audience has path":    func(claims jwt.MapClaims) { claims["aud"] = "https://external.integration.io/path" },
		"audience array":       func(claims jwt.MapClaims) { claims["aud"] = []string{"https://external.integration.io"} },
		"missing jti":          func(claims jwt.MapClaims) { delete(claims, "jti") },
		"invalid jti":          func(claims jwt.MapClaims) { claims["jti"] = " token-id " },
		"missing issued at":    func(claims jwt.MapClaims) { delete(claims, "iat") },
		"fractional issued at": func(claims jwt.MapClaims) { claims["iat"] = float64(testNow.Add(-time.Minute).Unix()) + 0.5 },
		"future issued at":     func(claims jwt.MapClaims) { claims["iat"] = testNow.Add(time.Minute).Unix() },
		"missing not before":   func(claims jwt.MapClaims) { delete(claims, "nbf") },
		"future not before":    func(claims jwt.MapClaims) { claims["nbf"] = testNow.Add(time.Minute).Unix() },
		"missing expiry":       func(claims jwt.MapClaims) { delete(claims, "exp") },
		"expired":              func(claims jwt.MapClaims) { claims["exp"] = testNow.Unix() },
		"expiry before issue":  func(claims jwt.MapClaims) { claims["exp"] = testNow.Add(-2 * time.Minute).Unix() },
		"missing account":      func(claims jwt.MapClaims) { delete(claims, "account_id") },
		"zero user":            func(claims jwt.MapClaims) { claims["user_id"] = int64(0) },
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t)
			claims := cloneClaims(fixture.claims)
			mutate(claims)
			rawToken := signClaims(t, claims, fixture.secret, jwt.SigningMethodHS256)
			if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Authenticate() error = %v, want ErrInvalidToken", err)
			}
			if fixture.repository.consumeCalls != 0 {
				t.Fatalf("consume calls = %d, want 0", fixture.repository.consumeCalls)
			}
		})
	}
}

func TestAuthenticateUnknownTenantIsUnauthorized(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	fixture.repository.findError = ErrUnknownTenant
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
	if _, err := fixture.authenticator.Authenticate(context.Background(), rawToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate() error = %v, want ErrInvalidToken", err)
	}
	if fixture.repository.consumeCalls != 0 {
		t.Fatalf("consume calls = %d, want 0", fixture.repository.consumeCalls)
	}
}

func TestAuthenticatePropagatesInfrastructureErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		findError    error
		consumeError error
	}{
		"lookup":  {findError: errors.New("database unavailable")},
		"consume": {consumeError: errors.New("database unavailable")},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAuthFixture(t)
			fixture.repository.findError = test.findError
			fixture.repository.consumeError = test.consumeError
			rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)
			_, err := fixture.authenticator.Authenticate(context.Background(), rawToken)
			if err == nil || errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrReplay) {
				t.Fatalf("Authenticate() error = %v, want infrastructure error", err)
			}
		})
	}
}

func TestNewAuthenticatorValidatesOptions(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	tests := map[string]struct {
		repository Repository
		opener     SecretOpener
		options    []Option
	}{
		"nil repository": {opener: fixture.authenticator.secrets},
		"nil opener":     {repository: fixture.repository},
		"nil option": {
			repository: fixture.repository,
			opener:     fixture.authenticator.secrets,
			options:    []Option{nil},
		},
		"nil clock": {
			repository: fixture.repository,
			opener:     fixture.authenticator.secrets,
			options:    []Option{WithClock(nil)},
		},
		"negative leeway": {
			repository: fixture.repository,
			opener:     fixture.authenticator.secrets,
			options:    []Option{WithLeeway(-time.Second)},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewAuthenticator(test.repository, test.opener, test.options...); err == nil {
				t.Fatal("NewAuthenticator() error = nil")
			}
		})
	}
}

func TestValidateStrictPayloadShapeRejectsDuplicateClaims(t *testing.T) {
	t.Parallel()

	payload := fmt.Sprintf(
		`{"iss":"https://subdomain.amocrm.ru","aud":"https://external.integration.io","jti":"%s",`+
			`"iat":%d,"nbf":%d,"exp":%d,"account_id":12345678,"account_id":12345678,`+
			`"user_id":87654321,"client_uuid":"%s"}`,
		testTokenID,
		testNow.Add(-time.Minute).Unix(),
		testNow.Add(-time.Minute).Unix(),
		testNow.Add(time.Minute).Unix(),
		testClientUUID,
	)
	raw := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	if err := validateStrictPayloadShape(raw); err == nil {
		t.Fatal("validateStrictPayloadShape() error = nil")
	}
}
