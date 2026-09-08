package corepolicy_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"github.com/golang-jwt/jwt/v5"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

type budgetTokens struct{ scope serviceapi.Scope }

func (f budgetTokens) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	return amocrm.AccessToken{InstallationID: f.scope.InstallationID, IntegrationID: f.scope.IntegrationID, AccountID: 42, AccountDomain: "fixture.amocrm.ru", Value: "fixture", TokenVersion: 1}, nil
}
func (f budgetTokens) RefreshIfCurrent(_ context.Context, a amocrm.AccessToken) (amocrm.AccessToken, error) {
	return a, nil
}
func (f budgetTokens) MarkReauthRequired(context.Context, uuid.UUID, int64) error { return nil }

type budgetHTTP struct {
	roleCalls, directoryCalls atomic.Int32
	revoked                   atomic.Bool
}

func (f *budgetHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	switch req.URL.Path {
	case "/api/v4/users/7":
		f.roleCalls.Add(1)
		body = fmt.Sprintf(`{"id":7,"rights":{"is_admin":%t,"is_active":true}}`, !f.revoked.Load())
	case "/api/v4/users":
		f.directoryCalls.Add(1)
		body = `{"_embedded":{"users":[{"id":7,"name":"Fixture admin","rights":{"group_id":3}}]}}`
	case "/api/v4/account":
		f.directoryCalls.Add(1)
		body = `{"id":42,"_embedded":{"users_groups":[{"id":3,"name":"Sales"}],"datetime_settings":{"timezone":"Europe/Moscow"}}}`
	default:
		return nil, fmt.Errorf("unexpected amoCRM request: %s", req.URL.Path)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

type budgetActivityRepo struct{ activity.Repository }

func (budgetActivityRepo) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	return activity.Defaults(), nil
}

type budgetEventsRepo struct{ crmevents.Repository }

func (budgetEventsRepo) Query(context.Context, serviceapi.Query, serviceapi.Principal) (serviceapi.QueryResult, error) {
	return serviceapi.QueryResult{ReadVersion: serviceapi.PresentationReadVersion, Events: []serviceapi.Event{}, Summaries: []serviceapi.UserSummary{}}, nil
}

// Exercise actual Activity -> Gateway + Events service authorization and the
// production database checker/amocrm.Client. Only product repositories and HTTP
// upstream are fixtures; counting a Policy mock would miss the original bug.
func TestPanelUsesOneLiveActorLookupAndImmediateDatabaseRevocation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'fixture','https://example.test/oauth')`, []any{scope.IntegrationID, uuid.NewString()}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, []any{scope.InstallationID, scope.IntegrationID}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled)VALUES($1,'activity',true)`, []any{scope.IntegrationID}},
		{`INSERT INTO activity_pilots(installation_id,enabled)VALUES($1,true)`, []any{scope.InstallationID}},
	}
	for _, s := range statements {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatal(err)
		}
	}
	upstream := &budgetHTTP{}
	client := amocrm.NewClient(&http.Client{Transport: upstream}, budgetTokens{scope})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := corepolicy.New(pool, client, key)
	if err != nil {
		t.Fatal(err)
	}
	core := corepolicy.ForCaller(policy, serviceapi.CoreService)
	gw := gateway.New(client, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	events := crmevents.NewWithRepository(budgetEventsRepo{}, corepolicy.ForCaller(policy, serviceapi.EventsService), gw, crmevents.Config{})
	product := activity.New(budgetActivityRepo{}, corepolicy.ForCaller(policy, serviceapi.ActivityService), events, gw)
	request := serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "same-request-id-is-not-a-cache-key", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanel)}
	var auth serviceapi.Auth
	for n := int32(1); n <= 2; n++ {
		auth, err = core.Issue(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		panel, err := product.Panel(ctx, serviceapi.Query{Auth: auth, From: 1000, To: 2000, Limit: 100})
		if err != nil || len(panel.Users) != 1 {
			t.Fatalf("panel=%+v err=%v", panel, err)
		}
		if got := upstream.roleCalls.Load(); got != n {
			t.Fatalf("request %d: actual /users/7 calls=%d, want %d (one fresh role lookup per Issue)", n, got, n)
		}
	}
	if got := upstream.directoryCalls.Load(); got != 2 {
		t.Fatalf("directory calls=%d want2 (account+users cached on second panel)", got)
	}
	t.Log("two panels: actual amoCRM role GETs=2 (1/request), directory GETs=2 (first request only)")
	// A signed role observation may finish its existing <=30s chain. Every NEW
	// request rechecks the role, even with the same request id and warm directory.
	upstream.revoked.Store(true)
	if _, err := core.Issue(ctx, request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("fresh Issue after admin revoke: %v", err)
	}
	if _, err := product.Panel(ctx, serviceapi.Query{Auth: auth, From: 1000, To: 2000, Limit: 100}); err != nil {
		t.Fatalf("still-valid delegation snapshot: %v", err)
	}
	if got := upstream.roleCalls.Load(); got != 3 {
		t.Fatalf("snapshot repeated role GET: %d", got)
	}
	upstream.revoked.Store(false)
	// The production checker cannot prolong a previously authorized snapshot.
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(auth.Token, claims); err != nil {
		t.Fatal(err)
	}
	claims["iat"] = time.Now().Add(-31 * time.Second).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = time.Now().Add(-time.Second).Unix()
	expired, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.Panel(ctx, serviceapi.Query{Auth: serviceapi.Auth{Token: expired}, From: 1000, To: 2000, Limit: 100}); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
		t.Fatalf("expired production delegation: %v", err)
	}
	for _, test := range []struct {
		name, disable, restore string
		code                   serviceapi.Code
	}{
		{"pilot", `UPDATE activity_pilots SET enabled=false WHERE installation_id=$1`, `UPDATE activity_pilots SET enabled=true WHERE installation_id=$1`, serviceapi.PermissionDenied},
		{"capability", `UPDATE integration_services SET enabled=false WHERE integration_id=(SELECT integration_id FROM installations WHERE id=$1)`, `UPDATE integration_services SET enabled=true WHERE integration_id=(SELECT integration_id FROM installations WHERE id=$1)`, serviceapi.PermissionDenied},
		{"integration", `UPDATE integrations SET status='disabled' WHERE id=(SELECT integration_id FROM installations WHERE id=$1)`, `UPDATE integrations SET status='active' WHERE id=(SELECT integration_id FROM installations WHERE id=$1)`, serviceapi.PermissionDenied},
		{"installation", `UPDATE installations SET status='disabled' WHERE id=$1`, `UPDATE installations SET status='active' WHERE id=$1`, serviceapi.PermissionDenied},
		{"reauth", `UPDATE installations SET status='reauth_required' WHERE id=$1`, `UPDATE installations SET status='active' WHERE id=$1`, serviceapi.ReauthRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, test.disable, scope.InstallationID); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.Exec(ctx, test.restore, scope.InstallationID); err != nil {
					t.Fatal(err)
				}
			}()
			if _, err := core.Issue(ctx, request); serviceapi.ErrorCode(err) != test.code {
				t.Fatalf("Issue: %v", err)
			}
			for _, grant := range request.Grants {
				if _, err := corepolicy.ForCaller(policy, grant.Audience).Validate(ctx, auth, grant.Audience, grant.Action); serviceapi.ErrorCode(err) != test.code {
					t.Fatalf("Validate %s: %v", grant.Audience, err)
				}
			}
		})
	}
	if got := upstream.roleCalls.Load(); got != 3 {
		t.Fatalf("DB revocation reached upstream: %d", got)
	}
}
