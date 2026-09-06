package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
)

func TestBootstrapDatabaseBindsKnownAccountAcrossIntegrations(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'fixture','https://fixture.test/oauth')`, id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(integration_id,account_id,account_domain,status)VALUES($1,42,'fixture.amocrm.ru','active')`, a); err != nil {
		t.Fatal(err)
	}
	lookup := bootstrapDatabase{pool}
	known, err := lookup.KnownAccount(ctx, b, "fixture.amocrm.ru")
	if err != nil || known != 42 {
		t.Fatalf("cross-integration known=%d err=%v", known, err)
	}
	if _, err := lookup.KnownAccount(ctx, uuid.New(), "fixture.amocrm.ru"); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("missing integration=%v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE integrations SET status='disabled' WHERE id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.KnownAccount(ctx, b, "fixture.amocrm.ru"); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("disabled integration=%v", err)
	}
	known, err = lookup.KnownAccount(ctx, a, "new.amocrm.ru")
	if err != nil || known != 0 {
		t.Fatalf("unknown first account=%d %v", known, err)
	}
}
