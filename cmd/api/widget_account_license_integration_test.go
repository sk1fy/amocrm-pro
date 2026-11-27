package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

func TestWidgetAccountLicenseIntake(t *testing.T) {
	ctx := t.Context()
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{4}, 32)}, 1)
	checkAccessTest(t, err)
	oauthStore := oauth.NewStore(pool, keys)
	pilot := newAccessPilot(t, ctx, oauthStore, "license-pilot", proofTestAccount)
	unmapped := newAccessPilot(t, ctx, oauthStore, "license-unmapped", proofTestAccount+1)
	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: []wire.Mapping{{
		Integration: pilot.integration.ID, ProductCode: pilot.product, Accounts: []string{pilot.selector()},
	}}})
	checkAccessTest(t, err)
	journal := corelifecycle.NewStore(pool)
	checkAccessTest(t, journal.Bootstrap(ctx, manifest))
	pilot.save(t, ctx, oauthStore)
	unmapped.save(t, ctx, oauthStore)

	authNow := time.Now
	const tokenLeeway = 3 * time.Second
	auth, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys, widgetauth.WithLeeway(tokenLeeway),
		widgetauth.WithClock(func() time.Time { return authNow() }))
	checkAccessTest(t, err)
	limiter, err := widgetlimit.New(widgetlimit.Config{
		IntegrationRate: 100, IntegrationBurst: 100, InstallationRate: 100, InstallationBurst: 100,
		InactiveTTL: time.Minute, MaxEntries: 16,
	}, nil)
	checkAccessTest(t, err)
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	router := chi.NewRouter()
	var reports []wire.AccountLicenseRequest
	var backendErr error
	registerWidgetAccountLicense(router, journal.AccountLicenseScope, func(_ context.Context, request wire.AccountLicenseRequest) error {
		reports = append(reports, request)
		return backendErr
	}, func(consume bool, next http.Handler) http.Handler {
		return protectWidgetRoute(auth, limiter, cors, consume, next)
	}, cors)
	request := func(path, body, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Origin", proofTestOrigin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Auth-Token", token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, r)
		if response.Code != want {
			t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body)
		}
		return response
	}
	freshToken := func(p accessPilot) string { return p.sdk(t, uuid.NewString())["X-Auth-Token"] }
	const hints = `{"tariff_id":22000000}`

	preflight := httptest.NewRequest(http.MethodOptions, widgetAccountLicensePath, nil)
	preflight.Header.Set("Origin", proofTestOrigin)
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "X-Auth-Token, Content-Type")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != proofTestOrigin {
		t.Fatalf("preflight status = %d, headers = %v", response.Code, response.Header())
	}
	request(widgetAccountLicensePath, hints, "wrong-token", http.StatusUnauthorized)
	request(widgetAccountLicensePath, `{"tariff_id":22000000,"installation_id":"attacker"}`, freshToken(pilot), http.StatusBadRequest)
	request(widgetAccountLicensePath+"?amo_account_id=7", hints, freshToken(pilot), http.StatusBadRequest)
	request(widgetAccountLicensePath, hints+strings.Repeat(" ", wire.MaxAccountLicenseHintsBody), freshToken(pilot), http.StatusBadRequest)
	request(widgetAccountLicensePath, hints, freshToken(unmapped), http.StatusNotFound)
	if len(reports) != 0 {
		t.Fatalf("invalid inputs reached Platform: %+v", reports)
	}

	token := freshToken(pilot)
	begin := time.Now()
	request(widgetAccountLicensePath, hints, token, http.StatusNoContent)
	request(widgetAccountLicensePath, hints, token, http.StatusUnauthorized)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	first := reports[0]
	if first.Scope != (wire.ActivationScope{Installation: pilot.installation, Integration: pilot.integration.ID,
		Account: pilot.account, Generation: 1, AuthorizationSequence: 1}) || first.ReceivedAt.Before(begin) ||
		first.ReceivedAt.After(time.Now()) || first.ReceivedAt.Location() != time.UTC || first.Hints.TariffID == nil ||
		*first.Hints.TariffID != 22000000 || first.Hints.PayType != nil || first.Hints.TrialEnd != nil {
		t.Fatalf("unexpected Platform report: %+v", first)
	}

	signedAt := func(issued int64) string {
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": proofTestOrigin, "aud": "https://core.example.test", "jti": uuid.NewString(), "iat": issued, "nbf": issued,
			"exp": issued + 90, "account_id": pilot.account, "user_id": 71, "client_uuid": pilot.client,
		}).SignedString([]byte(pilot.secret))
		checkAccessTest(t, err)
		return signed
	}
	staleToken := signedAt(time.Now().Add(-10 * time.Second).Unix())
	operator := integrations.NewStore(pool, keys)
	uninstall, err := operator.Apply(ctx, integrations.Command{
		Action: "uninstall", Actor: "license-test", Code: pilot.product, InstallationID: pilot.installation,
	})
	checkAccessTest(t, err)
	request(widgetAccountLicensePath, hints, freshToken(pilot), http.StatusUnauthorized)
	tx, err := pool.Begin(ctx)
	checkAccessTest(t, err)
	defer tx.Rollback(ctx)
	installation, err := corelifecycle.LockInstallation(ctx, tx, pilot.installation)
	checkAccessTest(t, err)
	checkAccessTest(t, journal.FinishUninstall(ctx, tx, installation, uninstall.UninstallAttempt))
	checkAccessTest(t, tx.Commit(ctx))
	pilot.save(t, ctx, oauthStore)
	request(widgetAccountLicensePath, hints, staleToken, http.StatusNotFound)
	var authorizationBody []byte
	checkAccessTest(t, pool.QueryRow(ctx, `
		SELECT l.body
		FROM platform_lifecycle_cursors c
		JOIN platform_lifecycle_ledger l ON l.installation_id=c.installation_id AND l.sequence=c.authorization_sequence
		WHERE c.installation_id=$1`, pilot.installation).Scan(&authorizationBody))
	authorization, err := wire.DecodeEvent(authorizationBody)
	checkAccessTest(t, err)
	authorizedAt := authorization.OccurredAt
	request(widgetAccountLicensePath, hints, signedAt(authorizedAt.Unix()), http.StatusServiceUnavailable)
	request(widgetAccountLicensePath, hints, signedAt(authorizedAt.Add(2*time.Second).Unix()), http.StatusServiceUnavailable)
	if len(reports) != 1 {
		t.Fatalf("stale token reached Platform: %+v", reports)
	}
	authNow = func() time.Time { return authorizedAt.Add(tokenLeeway + 2*time.Second) }
	backendErr = errors.New("Platform lifecycle event not applied yet")
	response = request(widgetAccountLicensePath, hints, signedAt(authNow().Unix()), http.StatusServiceUnavailable)
	if response.Header().Get("Retry-After") != "2" || !strings.Contains(response.Body.String(), `"retryable":true`) {
		t.Fatalf("Platform outage response is not retryable: %v %s", response.Header(), response.Body)
	}
	backendErr = nil
	request(widgetAccountLicensePath, hints, signedAt(authNow().Unix()), http.StatusNoContent)
	if len(reports) != 3 || reports[2].Scope.Generation != 2 || reports[2].Scope.AuthorizationSequence <= first.Scope.AuthorizationSequence {
		t.Fatalf("reinstall did not advance Platform report scope: %+v", reports)
	}
}
