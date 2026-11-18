package companycrm

import (
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestPinnedBindingRequiresActiveExactOwnerAndIgnoresActivity(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	b := binding()
	ctx := t.Context()
	for _, query := range []string{
		`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri,status) VALUES($1,'company-source','fixture-client','fixture','https://fixture.test/oauth','active')`,
	} {
		if _, err := pool.Exec(ctx, query, b.IntegrationID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,$3,'company.amocrm.test','active')`, b.InstallationID, b.IntegrationID, b.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oauth_credentials(installation_id,access_token_ciphertext,refresh_token_ciphertext,expires_at,token_version,key_version) VALUES($1,'fixture','fixture',now()+interval '1 hour',1,1)`, b.InstallationID); err != nil {
		t.Fatal(err)
	}
	store := &Store{Pool: pool, Binding: b}
	if got, err := store.Resolve(ctx); err != nil || got != b {
		t.Fatalf("binding without Activity grant=%+v err=%v", got, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE oauth_credentials SET token_version=token_version+1 WHERE installation_id=$1`, b.InstallationID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Resolve(ctx); err != nil || got.Revision != b.Revision {
		t.Fatalf("refresh changed binding=%+v err=%v", got, err)
	}
	store.Binding.AccountID++
	if _, err := store.Resolve(ctx); err == nil {
		t.Fatal("wrong account accepted")
	}
	store.Binding = b
	for _, state := range []string{"authorizing", "reauth_required", "disabled", "uninstalled"} {
		if _, err := pool.Exec(ctx, `UPDATE installations SET status=$2 WHERE id=$1`, b.InstallationID, state); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Resolve(ctx); err == nil {
			t.Fatalf("status %s accepted", state)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='active',settings='{"origin":"fixture"}' WHERE id=$1`, b.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(ctx); err == nil {
		t.Fatal("fixture credential accepted")
	}
}
