package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"testing"
)

type bootstrapLookupFake struct {
	integration uuid.UUID
	calls       int
}

func (f *bootstrapLookupFake) KnownAccount(_ context.Context, id uuid.UUID, domain string) (int64, error) {
	f.calls++
	if id != f.integration {
		return 0, serviceapi.Fail(serviceapi.PermissionDenied, "integration is not active")
	}
	if domain != "fixture.amocrm.ru" {
		return 0, serviceapi.Fail(serviceapi.InvalidArgument, "wrong canonical domain")
	}
	return 42, nil
}

type bootstrapAPIFake struct {
	calls       int
	integration uuid.UUID
	known       int64
}

func (f *bootstrapAPIFake) BootstrapAccount(_ context.Context, id uuid.UUID, known int64, domain, token string) (amocrm.Account, error) {
	f.calls++
	f.integration = id
	f.known = known
	return amocrm.Account{ID: 42, Subdomain: "fixture"}, nil
}
func TestBootstrapRestrictedToCoreAndLiveIntegration(t *testing.T) {
	id := uuid.New()
	lookup := &bootstrapLookupFake{integration: id}
	api := &bootstrapAPIFake{}
	service := NewBootstrapWithLookup(lookup, api)
	request := serviceapi.BootstrapAccountRequest{IntegrationID: id, AccountDomain: "https://FIXTURE.amocrm.ru/", AccessToken: "transient"}
	for _, identity := range []string{"activity", "crm-events", "gateway", ""} {
		if _, err := service.GetAccount(corepolicy.WithCaller(context.Background(), identity), request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("identity %q=%v", identity, err)
		}
	}
	if lookup.calls != 0 || api.calls != 0 {
		t.Fatal("unauthorized caller reached owner data")
	}
	result, err := service.GetAccount(corepolicy.WithCaller(context.Background(), "core"), request)
	if err != nil || result.ID != 42 || api.known != 42 || api.integration != id {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	request.IntegrationID = uuid.New()
	if _, err := service.GetAccount(corepolicy.WithCaller(context.Background(), "core"), request); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("missing integration=%v", err)
	}
	if api.calls != 1 {
		t.Fatal("denied integration reached HTTP")
	}
	request.IntegrationID = id
	request.AccountDomain = "http://127.0.0.1/secrets"
	if _, err := service.GetAccount(corepolicy.WithCaller(context.Background(), "core"), request); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
		t.Fatalf("arbitrary URL=%v", err)
	}
}
