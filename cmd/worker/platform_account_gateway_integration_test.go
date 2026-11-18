package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestPlatformAccountGatewayAcrossCoreAndProvider(t *testing.T) {
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	if certs == "" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("requires migrated Core test database and generated Platform mTLS certificates")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{7}, 32)}, 1)
	requireAccountGateway(t, err)
	oauthStore := oauth.NewStore(pool, keys)
	integration, err := oauthStore.EnsureIntegration(ctx, oauth.IntegrationInput{
		Code: "pilot-account", ClientID: uuid.NewString(), ClientSecret: "local-test-secret",
		RedirectURI: "https://core.test/oauth/amocrm/callback", WebhookEvents: []string{"add_lead"},
	})
	requireAccountGateway(t, err)
	const account = 9007199254740993
	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: []wire.Mapping{{
		Integration: integration.ID, ProductCode: "pilot-account", Accounts: []string{strconv.FormatInt(account, 10)},
	}}})
	requireAccountGateway(t, err)
	requireAccountGateway(t, corelifecycle.NewStore(pool).Bootstrap(ctx, manifest))
	save := func(account int64) uuid.UUID {
		t.Helper()
		result, err := oauthStore.SaveInstallation(ctx, integration, oauth.Account{ID: account}, "pilot.amocrm.ru", oauth.Token{
			AccessToken: "local-test-access", RefreshToken: "local-test-refresh", ExpiresIn: 7200,
		})
		requireAccountGateway(t, err)
		return result.ID
	}
	installation := save(account)
	unapproved := save(account + 1)

	var requests, mode atomic.Int32
	const throttled = 1
	const blocked = 2
	entered, release := make(chan struct{}, 1), make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer local-test-access" {
			t.Error("shared amoCRM client did not use the Core credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch mode.Load() {
		case throttled:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case blocked:
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Path {
		case "/api/v4/account":
			_, _ = fmt.Fprintf(w, `{"id":%d,"name":"Pilot Company","subdomain":"pilot","_embedded":{"datetime_settings":{"timezone":"Europe/Moscow"}}}`, account)
		case "/api/v4/users":
			_, _ = fmt.Fprint(w, `{"_page":1,"_page_count":1,"_total_items":1,"_embedded":{"users":[{"id":42,"name":"Local Staff","email":"excluded@example.test","rights":{"is_active":false,"is_admin":true}}]}}`)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	endpoint, err := url.Parse(upstream.URL)
	requireAccountGateway(t, err)
	httpClient := upstream.Client()
	httpClient.Transport = accountGatewayTestUpstream{origin: endpoint, transport: httpClient.Transport}
	tokens := oauth.NewTokenProvider(pool, keys, oauth.NewGateway(amocrm.NewOAuthClient(httpClient)))
	crm := amocrm.NewClient(httpClient, tokens)

	t.Setenv("PLATFORM_CORE_IDENTITY_DIR", filepath.Join(certs, "core"))
	server, addr, _, err := startPlatformAccountGateway(pool, crm, "127.0.0.1:0")
	requireAccountGateway(t, err)
	t.Cleanup(server.Stop)
	dial := func(identity string) *grpc.ClientConn {
		t.Helper()
		dir := filepath.Join(certs, identity)
		cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "core", false)
		requireAccountGateway(t, err)
		conn, err := grpc.NewClient(addr.String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		requireAccountGateway(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	connection := dial("platform")
	ready, err := grpc_health_v1.NewHealthClient(connection).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	requireAccountGateway(t, err)
	if ready.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("Core account Gateway is not ready: %v", ready)
	}
	platform := platformpb.NewCoreAccountObservationsClient(connection)
	req := &platformpb.ObserveAccountRequest{
		AmoAccountId: account, CoreInstallationId: installation.String(), IntegrationId: integration.ID.String(),
		Generation: 1, AuthorizationSequence: 1, Component: platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_LICENSE,
	}
	identity, err := platform.ObserveAccount(ctx, req)
	requireAccountGateway(t, err)
	if identity.GetIdentity().GetDisplayName() != "Pilot Company" || identity.GetIdentity().GetCurrentDomain() != "pilot.amocrm.ru" ||
		identity.GetIdentity().GetTimeZone() != "Europe/Moscow" || identity.GetObservedAt().AsTime().Before(time.Now().Add(-10*time.Second)) {
		t.Fatalf("scoped identity missing provider evidence: %v", identity)
	}
	usersRequest := proto.CloneOf(req)
	usersRequest.Component = platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS
	response, err := platform.ObserveAccount(ctx, usersRequest)
	requireAccountGateway(t, err)
	users := response.GetRoster().GetUsers()
	if len(users) != 1 || users[0].GetId() != 42 || users[0].GetName() != "Local Staff" || users[0].GetIsActive() || !users[0].GetIsAdmin() {
		t.Fatalf("unexpected roster evidence: %v", response)
	}
	for _, identity := range []string{"manager-bff", "core"} {
		other := platformpb.NewCoreAccountObservationsClient(dial(identity))
		if res, err := other.ObserveAccount(ctx, req); res != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s peer returned %v, %v", identity, res, err)
		}
	}
	wrongAccount, wrongPair, unmanaged, stale := proto.CloneOf(req), proto.CloneOf(req), proto.CloneOf(req), proto.CloneOf(req)
	wrongAccount.AmoAccountId++
	wrongPair.IntegrationId = uuid.NewString()
	unmanaged.AmoAccountId, unmanaged.CoreInstallationId = account+1, unapproved.String()
	stale.AuthorizationSequence++
	for _, invalid := range []*platformpb.ObserveAccountRequest{wrongAccount, wrongPair, unmanaged, stale} {
		if res, err := platform.ObserveAccount(ctx, invalid); res != nil || status.Code(err) != codes.PermissionDenied {
			t.Fatalf("wrong Core scope returned %v, %v", res, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("requests without current authority reached amoCRM: %d", requests.Load())
	}

	mode.Store(throttled)
	if res, err := platform.ObserveAccount(ctx, usersRequest); res != nil || status.Code(err) != codes.ResourceExhausted || gatewayRetryDelay(err) != 7*time.Second {
		t.Fatalf("provider throttle lost retry metadata: %v, %v", res, err)
	}
	mode.Store(blocked)
	finished := make(chan error, 1)
	go func() {
		res, err := platform.ObserveAccount(ctx, req)
		if res != nil {
			finished <- fmt.Errorf("disclosed an observation from superseded authority: %v", res)
			return
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := save(account); got != installation {
		t.Fatalf("reauthorization unexpectedly replaced installation %s", got)
	}
	release <- struct{}{}
	if err := <-finished; status.Code(err) != codes.PermissionDenied {
		t.Fatalf("successful provider fetch outlived Core authorization: %v", err)
	}
}

type accountGatewayTestUpstream struct {
	origin    *url.URL
	transport http.RoundTripper
}

func (u accountGatewayTestUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "pilot.amocrm.ru" {
		return nil, fmt.Errorf("unexpected upstream account authority %s", r.URL.Host)
	}
	local := r.Clone(r.Context())
	local.URL.Scheme, local.URL.Host = u.origin.Scheme, u.origin.Host
	return u.transport.RoundTrip(local)
}

func gatewayRetryDelay(err error) time.Duration {
	for _, detail := range status.Convert(err).Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok {
			return retry.RetryDelay.AsDuration()
		}
	}
	return 0
}

func requireAccountGateway(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
