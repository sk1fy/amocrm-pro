package integrations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestOperatorProvisioningKeepsIntegrationIsolationAndAuditsWithoutSecrets(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool, keys)
	uri := "https://example.test/oauth"
	secret := []byte("synthetic-secret-first")
	create := Command{Action: "create", Actor: "test-operator", Code: "widget-a", ClientID: uuid.NewString(), Secret: secret, RedirectURI: &uri, Services: []string{"lead-status"}}
	a, err := store.Apply(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	create.Code = "widget-b"
	create.ClientID = uuid.NewString()
	create.Services = []string{}
	b, err := store.Apply(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}
	create.Code = "widget-c"
	if _, err := store.Apply(ctx, create); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate client id: %v", err)
	}
	var bGrants int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM integration_services WHERE integration_id=$1`, b.ID).Scan(&bGrants); err != nil {
		t.Fatal(err)
	}
	if bGrants != 0 {
		t.Fatal("new integration received implicit capability")
	}
	for _, command := range []Command{
		{Action: "disable", Actor: "test-operator", Code: a.Code},
		{Action: "rotate-secret", Actor: "test-operator", Code: a.Code, Secret: []byte("synthetic-secret-rotated")},
		{Action: "set-service", Actor: "test-operator", Code: a.Code, Service: "lead-status", Enabled: false},
	} {
		if _, err := store.Apply(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	var statusA, statusB string
	var encryptedA, encryptedB []byte
	var versionA, versionB int
	if err := pool.QueryRow(ctx, `SELECT status,client_secret_ciphertext,client_secret_key_version FROM integrations WHERE id=$1`, a.ID).Scan(&statusA, &encryptedA, &versionA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status,client_secret_ciphertext,client_secret_key_version FROM integrations WHERE id=$1`, b.ID).Scan(&statusB, &encryptedB, &versionB); err != nil {
		t.Fatal(err)
	}
	if statusA != "disabled" || statusB != "active" {
		t.Fatal("disable/rotation changed another integration or reactivated target")
	}
	plainA, err := keys.Open(versionA, encryptedA, cryptox.IntegrationSecretAAD(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	plainB, err := keys.Open(versionB, encryptedB, cryptox.IntegrationSecretAAD(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	if string(plainA) != "synthetic-secret-rotated" || string(plainB) != string(secret) {
		t.Fatal("rotation crossed integration boundary")
	}
	if _, err := keys.Open(versionA, encryptedA, cryptox.IntegrationSecretAAD(b.ID)); err == nil {
		t.Fatal("integration AAD not enforced")
	}
	events := []string{"add_lead"}
	updatedURI := "https://example.test/new-callback"
	if _, err := store.Apply(ctx, Command{Action: "update", Actor: "test-operator", Code: a.Code, RedirectURI: &updatedURI, WebhookEvents: &events}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, Command{Action: "enable", Actor: "test-operator", Code: a.Code}); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM integration_services WHERE integration_id=$1 AND service_code='lead-status'`, a.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("enable integration restored revoked service")
	}
	if _, err := store.Apply(ctx, Command{Action: "set-service", Actor: "test-operator", Code: b.Code, Service: "lead-status", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var auditText string
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*),string_agg(row_to_json(audit_log)::text,'') FROM audit_log WHERE actor_type='operator'`).Scan(&count, &auditText); err != nil {
		t.Fatal(err)
	}
	if count != 8 || strings.Contains(auditText, "synthetic-secret") || strings.Contains(auditText, "https://") {
		t.Fatalf("unexpected audit count or sensitive metadata: count=%d", count)
	}
}

func TestOperatorMutationRollsBackWhenAuditFails(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	keys, _ := cryptox.NewKeyRing(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	store := NewStore(pool, keys)
	uri := "https://example.test/oauth"
	if _, err := pool.Exec(ctx, `CREATE FUNCTION operator_test_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.actor_type='operator' THEN RAISE EXCEPTION 'synthetic failure'; END IF; RETURN NEW; END; $$; CREATE TRIGGER operator_test_fail_audit BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION operator_test_fail_audit()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS operator_test_fail_audit ON audit_log;DROP FUNCTION IF EXISTS operator_test_fail_audit()`)
	})
	_, err := store.Apply(ctx, Command{Action: "create", Actor: "test-operator", Code: "widget-a", ClientID: uuid.NewString(), Secret: []byte("secret"), RedirectURI: &uri, Services: []string{"lead-status"}})
	if err == nil {
		t.Fatal("audit failure accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM integrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unaudited integration persisted")
	}
}
