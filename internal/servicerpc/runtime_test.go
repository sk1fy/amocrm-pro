package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"math/big"
	"net"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type checker struct {
	scope    serviceapi.Scope
	disabled atomic.Bool
}

func (c *checker) Check(ctx context.Context, s serviceapi.Scope, actor int64, system bool) error {
	if c.disabled.Load() || s != c.scope || (!system && actor != 7) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "not admitted")
	}
	return nil
}

type fakeAPI struct {
	err   error
	calls atomic.Int32
}

func (f *fakeAPI) ListEvents(ctx context.Context, id uuid.UUID, from, to int64, page, limit int) (amocrm.CRMEventPage, error) {
	f.calls.Add(1)
	if f.err != nil {
		return amocrm.CRMEventPage{}, f.err
	}
	return amocrm.CRMEventPage{Events: []amocrm.CRMEvent{{ID: "ev-1", CreatedAt: 1000, CreatedBy: 7, Type: "lead_added", EntityID: 42, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"x":1}]`)}}}, nil
}
func (f *fakeAPI) GetDirectory(context.Context, uuid.UUID) (amocrm.AccountDirectory, error) {
	return amocrm.AccountDirectory{Timezone: "Europe/Moscow", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Alice"}}}, nil
}

type certAuthority struct {
	cert  *x509.Certificate
	key   ed25519.PrivateKey
	roots *x509.CertPool
}

func newCA(t *testing.T) certAuthority {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return certAuthority{cert, key, roots}
}
func (c certAuthority) config(t *testing.T, identity string, server bool) *tls.Config {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	u, _ := url.Parse("spiffe://amocrm-pro/" + identity)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, URIs: []*url.URL{u}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, RootCAs: c.roots, ServerName: "localhost"}
	if server {
		config.ClientCAs = c.roots
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config
}
func start(t *testing.T, ca certAuthority, endpoints *Endpoints) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(ca.config(t, "gateway", true), endpoints)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}
func dialTest(t *testing.T, ca certAuthority, address, identity string) *Clients {
	t.Helper()
	c, err := Dial(context.Background(), address, ca.config(t, identity, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func TestActualMTLSGatewayAndPolicyLocalParity(t *testing.T) {
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	gw := gateway.New(api, corepolicy.ForCaller(policy, "gateway"))
	address := start(t, ca, &Endpoints{Policy: policy, Gateway: gw})
	core := dialTest(t, ca, address, "core")
	remote := dialTest(t, ca, address, "crm-events")
	activity := dialTest(t, ca, address, "activity")
	if err := core.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	background := serviceapi.IssueRequest{Scope: scope, System: true, Consumer: "activity", RequestID: "sync", Grants: []serviceapi.Grant{{Audience: "gateway", Action: "events"}, {Audience: "crm-events", Action: "sync"}}}
	auth, err := remote.Policy.Issue(context.Background(), background)
	if err != nil {
		t.Fatal(err)
	}
	request := serviceapi.EventPageRequest{Auth: auth, From: 1000, To: 2000, Page: 1, Limit: 100}
	localResult, err := gw.Events(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	remoteResult, err := remote.Gateway.Events(context.Background(), request)
	if err != nil || !reflect.DeepEqual(localResult, remoteResult) {
		t.Fatalf("local=%+v remote=%+v err=%v", localResult, remoteResult, err)
	}
	// A valid signed collector context cannot replace the authenticated mTLS identity.
	if _, err := activity.Gateway.Events(context.Background(), request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("wrong caller %v", err)
	}
	for _, target := range []serviceapi.Gateway{gw, remote.Gateway} {
		bad := request
		bad.Auth.Token = "forged"
		if _, err := target.Events(context.Background(), bad); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
			t.Fatalf("forgery %v", err)
		}
		bad = request
		bad.Limit = 250
		if _, err := target.Events(context.Background(), bad); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("bounds %v", err)
		}
	}
	actorRequest := serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: "activity", RequestID: "panel", Grants: serviceapi.UserGrants()}
	actor, err := core.Policy.Issue(context.Background(), actorRequest)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := activity.Gateway.Users(context.Background(), serviceapi.UsersRequest{Auth: actor})
	if err != nil || len(directory.Users) != 1 {
		t.Fatalf("directory=%+v err=%v", directory, err)
	}
	if _, err := activity.Policy.Issue(context.Background(), actorRequest); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("Activity issuer %v", err)
	}
	actorRequest.ActorID = 8
	if _, err := core.Policy.Issue(context.Background(), actorRequest); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("nonadmin %v", err)
	}
	if _, err := activity.Policy.Validate(context.Background(), actor, "crm-events", "read"); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("cross-audience %v", err)
	}
	check.disabled.Store(true)
	calls := api.calls.Load()
	for _, target := range []serviceapi.Gateway{gw, remote.Gateway} {
		if _, err := target.Events(context.Background(), request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("disable %v", err)
		}
	}
	if api.calls.Load() != calls {
		t.Fatal("revoked source reached upstream")
	}
	rogue := dialTest(t, ca, address, "unknown")
	if err := rogue.Ready(context.Background()); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("unknown service identity %v", err)
	}
}
func TestRPCErrorRetryAfterAndCancellation(t *testing.T) {
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, _ := corepolicy.NewWithChecker(check, key)
	api := &fakeAPI{err: &amocrm.APIError{Kind: amocrm.ErrorRateLimited, Retryable: true, RetryAfter: 13 * time.Second}}
	gw := gateway.New(api, corepolicy.ForCaller(policy, "gateway"))
	address := start(t, ca, &Endpoints{Policy: policy, Gateway: gw})
	remote := dialTest(t, ca, address, "crm-events")
	auth, err := remote.Policy.Issue(context.Background(), serviceapi.IssueRequest{Scope: scope, System: true, Consumer: "activity", RequestID: "sync", Grants: []serviceapi.Grant{{Audience: "gateway", Action: "events"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := serviceapi.EventPageRequest{Auth: auth, From: 1000, To: 2000, Page: 1, Limit: 100}
	_, err = remote.Gateway.Events(context.Background(), request)
	domain, ok := err.(*serviceapi.Error)
	if !ok || domain.Code != serviceapi.ResourceExhausted || domain.RetryAfter != 13*time.Second {
		t.Fatalf("mapped429=%v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err = remote.Gateway.Events(ctx, request)
	if serviceapi.ErrorCode(err) != serviceapi.DeadlineExceeded {
		t.Fatalf("deadline %v", err)
	}
}
func TestProtobufRoundTripAllPanelStateFields(t *testing.T) {
	input := serviceapi.Panel{Coverage: "partial", Users: []serviceapi.User{{ID: 7, Name: "A", GroupID: 9, GroupName: "Sales"}}, Timezone: "Europe/Moscow", Settings: serviceapi.Settings{InitialDays: 2, RetentionDays: 7}, Data: serviceapi.QueryResult{NextCursor: "cursor", Events: []serviceapi.Event{{ID: "e", CreatedAt: 1000, CreatedBy: 7, ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[{"a":3}]`)}}, Summaries: []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1, LastEventAt: 1000}}, Status: serviceapi.SyncStatus{Verification: "stabilized_api_scan", Enabled: true, State: "running", HistoryFrom: 1, VerifiedFrom: 2, VerifiedThrough: 3, WindowFrom: 4, WindowTo: 5, NextPage: 6, LastSuccessAt: 7, LastEventAt: 8, LagSeconds: 9, ErrorCode: "rate_limited", ReauthRequired: true}}}
	output := fromPanel(toPanel(input))
	if !reflect.DeepEqual(input, output) {
		t.Fatalf("roundtrip=%+v", output)
	}
}

func TestReadinessReflectsDependencyFailure(t *testing.T) {
	ca := newCA(t)
	var unavailable atomic.Bool
	address := start(t, ca, &Endpoints{Ready: func(context.Context) error {
		if unavailable.Load() {
			return serviceapi.Fail(serviceapi.Unavailable, "database unavailable")
		}
		return nil
	}})
	client := dialTest(t, ca, address, "core")
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	if err := client.Ready(context.Background()); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("readiness=%v", err)
	}
}

func TestConnectDoesNotWaitForUnavailableDependency(t *testing.T) {
	ca := newCA(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	started := time.Now()
	client, err := Connect(address, ca.config(t, "core", false))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("Connect blocked on optional dependency for %s", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = client.Ready(ctx)
	code := serviceapi.ErrorCode(err)
	if err == nil || (code != serviceapi.Unavailable && code != serviceapi.DeadlineExceeded) {
		t.Fatalf("unavailable bounded call=%v", err)
	}
}
