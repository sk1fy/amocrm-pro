package corepolicy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

// Provisioning, OAuth persistence, widget identity and employee admission must
// agree on one installation. CRM responses and credentials here are synthetic;
// this test does not claim to exercise an installed amoCRM SDK.
func TestActivityRegistrationAdmitsEmployeeOnlyWithinProvisionedInstallation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	registry := integrations.NewStore(pool, keys)
	redirect := "https://backend.example.invalid/oauth/amocrm/callback"
	clientID := uuid.NewString()
	secret := []byte("fixture-activity-client-secret")
	registered, err := registry.Apply(ctx, integrations.Command{
		Action: "create", Actor: "fixture-operator", Code: "rkrs_activity_panel",
		ClientID: clientID, Secret: secret, RedirectURI: &redirect, Services: []string{"activity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oauthStore := oauth.NewStore(pool, keys)
	integration, err := oauthStore.FindIntegrationByCode(ctx, registered.Code)
	if err != nil || integration.ID != registered.ID || integration.ClientID != clientID {
		t.Fatalf("provisioned OAuth identity mismatch: %v", err)
	}
	installation, err := oauthStore.SaveInstallation(ctx, integration, oauth.Account{ID: 42},
		"fixture-activity.amocrm.test", oauth.Token{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiresIn: 3600})
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "https://fixture-activity.amocrm.test", "aud": "https://backend.example.invalid",
		"client_uuid": clientID, "account_id": 42, "user_id": 7, "jti": uuid.NewString(),
		"iat": now.Add(-time.Second).Unix(), "nbf": now.Add(-time.Second).Unix(), "exp": now.Add(time.Minute).Unix(),
	}).SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authenticator.Authenticate(ctx, raw)
	if err != nil || principal.IntegrationID != registered.ID || principal.InstallationID != installation.ID || principal.AccountID != 42 {
		t.Fatalf("widget identity does not match provisioned installation: %v", err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	reader := &roleReader{active: true, admin: false}
	policy, err := NewWithChecker(&databaseChecker{pool, reader}, key)
	if err != nil {
		t.Fatal(err)
	}
	core := ForCaller(policy, serviceapi.CoreService)
	request := serviceapi.IssueRequest{
		Scope:   serviceapi.Scope{IntegrationID: principal.IntegrationID, InstallationID: principal.InstallationID},
		ActorID: principal.UserID, Kind: serviceapi.PrincipalKindEmployee, Consumer: serviceapi.ActivityService,
		RequestID: uuid.NewString(), Grants: []serviceapi.Grant{{Audience: serviceapi.ActivityService, Action: serviceapi.ActionSessions}},
	}
	assertDenied := func(label string) {
		t.Helper()
		if _, err := core.Issue(ctx, request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("%s: want permission denied, got %v", label, err)
		}
	}
	assertDenied("pilot defaults off even with activity grant")
	if err := activitybridge.SetPilot(ctx, pool, installation.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := core.Issue(ctx, request)
	if err != nil {
		t.Fatalf("active non-admin employee rejected: %v", err)
	}
	delegated, err := core.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionSessions)
	if err != nil || delegated.ActorID != 7 || delegated.Kind != serviceapi.PrincipalKindEmployee || delegated.Scope != request.Scope {
		t.Fatalf("employee delegation mismatch: %v", err)
	}
	request.Grants[0].Action = serviceapi.ActionSettings
	assertDenied("employee cannot elevate to settings")
	request.Grants[0].Action = serviceapi.ActionSessions
	request.IntegrationID = uuid.New()
	assertDenied("another integration cannot use this installation")
	request.IntegrationID = registered.ID
	reader.active = false
	assertDenied("inactive employee")
	reader.active = true
	if _, err := registry.Apply(ctx, integrations.Command{Action: "set-service", Actor: "fixture-operator", Code: registered.Code, Service: "activity", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	assertDenied("revoked capability")
	if _, err := core.Validate(ctx, auth, serviceapi.ActivityService, serviceapi.ActionSessions); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("existing delegation survived capability revocation: %v", err)
	}
}
