package servicerpc

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type bootstrapLookup struct{ integration uuid.UUID }

func (l bootstrapLookup) KnownAccount(_ context.Context, integration uuid.UUID, domain string) (int64, error) {
	if integration != l.integration {
		return 0, serviceapi.Fail(serviceapi.PermissionDenied, "integration is not active")
	}
	return 42, nil
}

type bootstrapHTTP struct {
	status atomic.Int32
	calls  atomic.Int32
}

func (h *bootstrapHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	h.calls.Add(1)
	return &http.Response{StatusCode: int(h.status.Load()), Header: http.Header{"Retry-After": []string{"9"}}, Body: io.NopCloser(strings.NewReader(`{"id":42,"subdomain":"fixture"}`)), Request: r}, nil
}
func TestBootstrapAccountMTLSCoreOnlyAndUpstreamErrors(t *testing.T) {
	ca := newCA(t)
	integration := uuid.New()
	httpTransport := &bootstrapHTTP{}
	httpTransport.status.Store(200)
	sharedClient := amocrm.NewClient(&http.Client{Transport: httpTransport}, nil)
	owner := gateway.NewBootstrapWithLookup(bootstrapLookup{integration}, sharedClient)
	address := start(t, ca, &Endpoints{BootstrapAccount: owner})
	request := serviceapi.BootstrapAccountRequest{IntegrationID: integration, AccountDomain: "fixture.amocrm.ru", AccessToken: "candidate-never-stored"}
	core := dialTest(t, ca, address, "core")
	account, err := core.BootstrapAccount.GetAccount(context.Background(), request)
	if err != nil || account.ID != 42 {
		t.Fatalf("bootstrap=%+v err=%v", account, err)
	}
	for _, identity := range []string{"activity", "crm-events", "gateway"} {
		client := dialTest(t, ca, address, identity)
		if _, err := client.BootstrapAccount.GetAccount(context.Background(), request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("%s bootstrap=%v", identity, err)
		}
	}
	if httpTransport.calls.Load() != 1 {
		t.Fatal("product invoked Core bootstrap HTTP")
	}
	httpTransport.status.Store(429)
	_, err = core.BootstrapAccount.GetAccount(context.Background(), request)
	domain, ok := err.(*serviceapi.Error)
	if !ok || domain.Code != serviceapi.ResourceExhausted || domain.RetryAfter != 9*time.Second {
		t.Fatalf("429 RPC=%v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := core.BootstrapAccount.GetAccount(ctx, request); serviceapi.ErrorCode(err) != serviceapi.DeadlineExceeded {
		t.Fatalf("cancellation RPC=%v", err)
	}
}
