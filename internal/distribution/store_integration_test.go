package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"testing"
	"time"
)

func TestDurableScopeBindingReplayLifecycleAndMapping(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	s := NewStore(pool)
	integration, install, company := uuid.New(), uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,'x','https://service.test/oauth')`, integration, uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'test.amocrm.ru','active')`, install, integration)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{ID: uuid.New(), CompanyID: company, InstallationID: install, IntegrationID: integration, AccountID: 123, Revision: 1, IntentID: uuid.New()}
	scope := Scope{"key", company, install}
	p := widgetauth.Principal{IntegrationID: integration, InstallationID: install, AccountID: 123, UserID: 7, TokenID: uuid.NewString(), Issuer: "https://test.amocrm.ru", TokenRetainUntil: time.Now().Add(time.Minute)}
	if _, e := s.Bind(ctx, scope, b, p, time.Now().Add(10*time.Minute)); !errors.Is(e, services.ErrNotEnabled) {
		t.Fatalf("off capability: %v", e)
	}
	_, err = pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'lead-distribution',true)`, integration)
	if err != nil {
		t.Fatal(err)
	}
	wrong := b
	wrong.CompanyID = uuid.New()
	if _, e := s.Bind(ctx, scope, wrong, p, time.Now().Add(10*time.Minute)); !errors.Is(e, ErrDenied) {
		t.Fatal("crosscompany allowed")
	}
	wrong = b
	wrong.AccountID = 124
	if _, e := s.Bind(ctx, scope, wrong, p, time.Now().Add(10*time.Minute)); !errors.Is(e, ErrDenied) {
		t.Fatal("crossaccount allowed")
	}
	saved, e := s.Bind(ctx, scope, b, p, time.Now().Add(10*time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	if saved.State != "active" {
		t.Fatal(saved)
	}
	if _, e = s.Bind(ctx, scope, b, p, time.Now().Add(10*time.Minute)); !errors.Is(e, ErrConflict) {
		t.Fatal("repeat insert")
	}
	if _, e = s.Get(ctx, Scope{"key", uuid.New(), install}, b.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("foreigncompany disclosed")
	}
	employee := uuid.New()
	if e = s.ReplaceMappings(ctx, b, []Mapping{{employee, 7}}, 1); e != nil {
		t.Fatal(e)
	}
	if e = s.Mapped(ctx, b, employee, 7); e != nil {
		t.Fatal(e)
	}
	if e = s.Mapped(ctx, b, employee, 8); !errors.Is(e, ErrDenied) {
		t.Fatal("wrongactor allowed")
	}
	_, err = pool.Exec(ctx, `UPDATE installations SET status='reauth_required' WHERE id=$1`, install)
	if err != nil {
		t.Fatal(err)
	}
	status, e := s.Get(ctx, scope, b.ID)
	if e != nil || status.State != "reauth_required" {
		t.Fatalf("reauth%v/%v", status, e)
	}
	if e = s.Require(ctx, b); !errors.Is(e, services.ErrNotEnabled) {
		t.Fatal("reauth allows admission")
	}
	_, err = pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, install)
	if err != nil {
		t.Fatal(err)
	}
	if e = s.Require(ctx, b); e != nil {
		t.Fatal(e)
	}
	if e = s.Revoke(ctx, scope, b.ID); e != nil {
		t.Fatal(e)
	}
	old, e := s.Get(ctx, scope, b.ID)
	if e != nil || old.State != "revoked" {
		t.Fatal(old, e)
	}
	if e = s.Require(ctx, old); !errors.Is(e, ErrDenied) {
		t.Fatal("revoked allowed")
	}
	b.ID = uuid.New()
	b.IntentID = uuid.New()
	p.TokenID = uuid.NewString()
	if _, e = s.Bind(ctx, scope, b, p, time.Now().Add(10*time.Minute)); e != nil {
		t.Fatal("rebind", e)
	}
	// Grants and nonces survive replicas; same nonce is denied.
	_, err = pool.Exec(ctx, `INSERT INTO distribution_service_grants(key_id,company_id,installation_id,enabled) VALUES('key',$1,$2,true)`, company, install)
	if err != nil {
		t.Fatal(err)
	}
	n := uuid.New()
	if e = s.AuthorizeRequest(ctx, scope, n, time.Now().Add(5*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e = NewStore(pool).AuthorizeRequest(ctx, scope, n, time.Now().Add(5*time.Minute)); !errors.Is(e, ErrDenied) {
		t.Fatal("replayednonce")
	}
	scope.CompanyID = uuid.New()
	if e = s.AuthorizeRequest(ctx, scope, uuid.New(), time.Now().Add(5*time.Minute)); !errors.Is(e, ErrDenied) {
		t.Fatal("foreigngrant")
	}
}
