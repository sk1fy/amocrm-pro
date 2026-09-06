package oauth

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestOAuthAccountReaderUsesConsumedStateAndNoLocalFallback(t *testing.T) {
	pool := testkitPostgresForServiceCallback(t)
	keys := oauthTestKeyRing(t)
	store := NewStore(pool, keys)
	integration := oauthTestIntegration(t, store)
	var calls int
	unavailable := errors.New("shared account gateway unavailable")
	remoteError := error(nil)
	legacy := &serviceCallbackGateway{
		exchangeCode: func(context.Context, string, string, string, string, string) (Token, error) {
			return Token{AccessToken: "transient-access", RefreshToken: "transient-refresh", ExpiresIn: 3600}, nil
		},
		getAccount: func(context.Context, string, string) (Account, error) {
			t.Fatal("account lookup bypassed shared Gateway")
			return Account{}, nil
		},
	}
	gateway := WithAccountReader(legacy, func(ctx context.Context, id uuid.UUID, domain, access string) (Account, error) {
		calls++
		if id != integration.ID || domain != "reader.amocrm.ru" || access != "transient-access" {
			t.Fatal("trusted OAuth identity or transient token changed")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("account RPC lost callback deadline")
		}
		return Account{ID: 991, Subdomain: "reader"}, remoteError
	})
	service := NewService(store, keys, gateway, time.Minute, 5*time.Second)
	state, _, err := store.CreateState(context.Background(), integration.ID, "/widget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Callback(context.Background(), state, "code", "https://reader.amocrm.ru")
	if err != nil || result.AccountID != 991 {
		t.Fatalf("callback=%+v error=%v", result, err)
	}
	remoteError = unavailable
	state, _, err = store.CreateState(context.Background(), integration.ID, "/widget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Callback(context.Background(), state, "new-code", "https://reader.amocrm.ru"); !errors.Is(err, unavailable) {
		t.Fatalf("gateway failure bypassed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("reader calls=%d", calls)
	}
	if _, err = gateway.GetAccount(context.Background(), "reader.amocrm.ru", "transient-access"); err == nil {
		t.Fatal("missing trusted state context accepted")
	}
}
