package widgetauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestFailureReasonForAudienceMismatch(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	claims := cloneClaims(fixture.claims)
	claims["aud"] = "https://attacker.example"
	rawToken := signClaims(t, claims, fixture.secret, jwt.SigningMethodHS256)

	_, err := fixture.authenticator.Verify(context.Background(), rawToken)
	if err == nil {
		t.Fatal("Verify() error = nil")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify() error = %v, want ErrInvalidToken", err)
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Verify() error = %T, want *Failure", err)
	}
	if failure.Reason != reasonClaimsInvalid {
		t.Fatalf("reason = %q, want %q", failure.Reason, reasonClaimsInvalid)
	}
	if failure.AudienceMatch == nil || *failure.AudienceMatch {
		t.Fatalf("aud_match = %v, want false", failure.AudienceMatch)
	}
	if failure.IssuerMatch != nil {
		t.Fatalf("iss_match = %v, want nil (not evaluated)", failure.IssuerMatch)
	}
}

func TestFailureReasonForUnknownTenant(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	fixture.repository.findError = ErrUnknownTenant
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)

	_, err := fixture.authenticator.Verify(context.Background(), rawToken)
	if err == nil {
		t.Fatal("Verify() error = nil")
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Verify() error = %T, want *Failure", err)
	}
	if failure.Reason != reasonTenantNotFound {
		t.Fatalf("reason = %q, want %q", failure.Reason, reasonTenantNotFound)
	}
}

func TestFailureReasonForWrongSignature(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	rawToken := signClaims(t, fixture.claims, []byte("wrong signing secret"), jwt.SigningMethodHS256)

	_, err := fixture.authenticator.Verify(context.Background(), rawToken)
	if err == nil {
		t.Fatal("Verify() error = nil")
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Verify() error = %T, want *Failure", err)
	}
	if failure.Reason != reasonSignatureInvalid {
		t.Fatalf("reason = %q, want %q", failure.Reason, reasonSignatureInvalid)
	}
}

func TestMiddlewareLogsSafeReasonAndRequestIDOnly(t *testing.T) {
	t.Parallel()

	fixture := newAuthFixture(t)
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	authenticator, err := NewAuthenticator(
		fixture.repository,
		fixture.authenticator.secrets,
		WithClock(fixture.authenticator.clock),
		WithLeeway(fixture.authenticator.leeway),
		WithMaxLifetime(fixture.authenticator.maxLifetime),
		WithLogger(logger),
	)
	if err != nil {
		t.Fatal(err)
	}
	rawToken := signClaims(t, fixture.claims, fixture.secret, jwt.SigningMethodHS256)

	handler := authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called")
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/widget/bootstrap", nil)
	request.Header.Set("X-Auth-Token", rawToken)
	request.Header.Set("Authorization", "Bearer "+rawToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}

	var entry map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &entry); err != nil {
		t.Fatalf("log is not valid JSON: %v; body=%s", err, buffer.String())
	}
	if entry["reason_code"] != reasonHeadersConflict {
		t.Fatalf("reason_code = %v, want %q", entry["reason_code"], reasonHeadersConflict)
	}
	if entry["request_id"] == nil || entry["request_id"] == "" {
		t.Fatal("request_id missing from log")
	}
	logText := buffer.String()
	if strings.Contains(logText, rawToken) {
		t.Fatal("log contains raw token")
	}
	if strings.Contains(logText, testSecret) {
		t.Fatal("log contains signing secret")
	}
}
