package taskcontrol

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

type lifecycleCRM struct {
	err    error
	before func()
}

func (f *lifecycleCRM) CheckTaskControlAuthorization(_ context.Context, _ uuid.UUID, _ int64) error {
	if f.before != nil {
		f.before()
	}
	return f.err
}

func TestDisconnectSignature(t *testing.T) {
	m := hmac.New(sha256.New, []byte("secret"))
	m.Write([]byte("client|123"))
	sig := hex.EncodeToString(m.Sum(nil))
	if !validDisconnectSignature("client", "123", sig, []byte("secret")) || validDisconnectSignature("client", "124", sig, []byte("secret")) || validDisconnectSignature("client", "123", sig, []byte("other")) {
		t.Fatal("signature scope")
	}
}

func TestDisconnectReconciliation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	id, integration, company := uuid.New(), uuid.New(), uuid.New()
	client := uuid.NewString()
	keys, _ := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{31}, cryptox.KeySize)}, 1)
	secret := []byte("synthetic-disconnect-secret")
	cipher, version, _ := keys.Seal(secret, cryptox.IntegrationSecretAAD(integration))
	for _, q := range []struct {
		s string
		a []any
	}{
		{`INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri) VALUES($1,'disconnect-test',$2,$3,$4,'https://example.test/callback')`, []any{integration, client, cipher, version}},
		{`INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'task-control',true)`, []any{integration}},
		{`INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,123,'tenant.amocrm.ru','active')`, []any{id, integration}},
		{`INSERT INTO task_control_service_grants(key_id,company_id,account_id,integration_id,enabled) VALUES('key',$1,123,$2,true)`, []any{company, integration}},
	} {
		if _, err := pool.Exec(ctx, q.s, q.a...); err != nil {
			t.Fatal(err)
		}
	}
	crm := &lifecycleCRM{}
	h := &Lifecycle{Store: &Store{Pool: pool}, Keys: keys, CRM: crm}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(client + "|123"))
	signature := hex.EncodeToString(m.Sum(nil))
	hook := func(sig string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/disconnect?client_uuid="+client+"&account_id=123&signature="+sig, nil))
		return w.Code
	}
	status := func() Status {
		v, e := h.Store.Read(ctx, "key", company, 123, uuid.New(), time.Now())
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if hook("00") != 403 || status().State != "installed" {
		t.Fatal("forged signal changed state")
	}
	if hook(signature) != 202 || status().State != "unknown" {
		t.Fatal("signal is not proof of uninstall")
	}
	crm.err = errors.New("timeout")
	if e := h.Reconcile(ctx, id); e != nil {
		t.Fatal(e)
	}
	if status().State != "unknown" {
		t.Fatal("timeout became uninstall")
	}
	crm.err = nil
	if e := h.Reconcile(ctx, id); e != nil {
		t.Fatal(e)
	}
	before := status()
	if before.State != "installed" {
		t.Fatal(before)
	}
	_ = hook(signature)
	crm.err = &amocrm.APIError{Kind: amocrm.ErrorUnauthorized, StatusCode: 401}
	if e := h.Reconcile(ctx, id); e != nil {
		t.Fatal(e)
	}
	after := status()
	if after.State != "notInstalled" || after.Generation != before.Generation+1 {
		t.Fatal(after)
	}
	_ = hook(signature)
	if status().Generation != after.Generation {
		t.Fatal("duplicate advanced generation")
	}
	// Fresh authorization supersedes the old signal and fences a delayed probe.
	if _, e := pool.Exec(ctx, `UPDATE task_control_installation_versions SET generation=generation+1,evidence='registry',signal_pending=false WHERE installation_id=$1`, id); e != nil {
		t.Fatal(e)
	}
	_ = hook(signature)
	crm.before = func() {
		if _, e := pool.Exec(ctx, `UPDATE task_control_installation_versions SET generation=generation+1,evidence='registry',signal_pending=false WHERE installation_id=$1`, id); e != nil {
			t.Fatal(e)
		}
	}
	if e := h.Reconcile(ctx, id); e != nil {
		t.Fatal(e)
	}
	if status().State != "installed" {
		t.Fatal("late probe overwrote reauthorization")
	}
	before = status()
	if _, e := pool.Exec(ctx, `UPDATE task_control_service_grants SET enabled=false WHERE company_id=$1`, company); e != nil {
		t.Fatal(e)
	}
	if _, e := h.Store.Read(ctx, "key", company, 123, uuid.New(), time.Now()); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `UPDATE task_control_service_grants SET enabled=true WHERE company_id=$1`, company); e != nil {
		t.Fatal(e)
	}
	if status().Generation != before.Generation+1 {
		t.Fatal("grant revoke did not fence sessions")
	}
	before = status()
	_, err := oauth.NewStore(pool, keys).SaveInstallation(ctx, oauth.Integration{ID: integration, WebhookEvents: []string{}}, oauth.Account{ID: 123}, "tenant.amocrm.ru", oauth.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresIn: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if status().Generation != before.Generation+1 || status().State != "installed" {
		t.Fatal("active-to-active OAuth did not fence old identity")
	}
}
