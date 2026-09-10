package integrations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

func TestDisableIntegrationIsNotUninstall(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	store, integrationID := newLifecycleStore(t, pool)
	installationID := insertLifecycleInstallation(t, pool, integrationID)
	if _, err := store.Apply(ctx, Command{Action: "disable", Actor: "test-operator", Code: "widget-a"}); err != nil {
		t.Fatal(err)
	}
	var integrationStatus, installationStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM integrations WHERE id=$1`, integrationID).Scan(&integrationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1`, installationID).Scan(&installationStatus); err != nil {
		t.Fatal(err)
	}
	if integrationStatus != "disabled" || installationStatus != "active" {
		t.Fatalf("disable integration changed installation: integration=%s installation=%s", integrationStatus, installationStatus)
	}
	if _, err := store.Apply(ctx, Command{Action: "enable", Actor: "test-operator", Code: "widget-a"}); err != nil {
		t.Fatal(err)
	}
	result, err := store.Apply(ctx, Command{Action: "uninstall", Actor: "test-operator", Code: "widget-a", InstallationID: installationID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "uninstalled" || result.Action != "installation.uninstall" {
		t.Fatalf("unexpected uninstall result: %#v", result)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM integrations WHERE id=$1`, integrationID).Scan(&integrationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1`, installationID).Scan(&installationStatus); err != nil {
		t.Fatal(err)
	}
	if integrationStatus != "active" || installationStatus != "uninstalled" {
		t.Fatalf("uninstall changed integration: integration=%s installation=%s", integrationStatus, installationStatus)
	}
}

func TestUninstallPreservesOwnerDataAndIsDeniedByExistingGuards(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	store, integrationID := newLifecycleStore(t, pool)
	installationID := insertLifecycleInstallation(t, pool, integrationID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO oauth_credentials (
			installation_id, access_token_ciphertext, refresh_token_ciphertext,
			expires_at, token_version, key_version
		) VALUES ($1, decode('00','hex'), decode('00','hex'), now() + interval '1 hour', 1, 1)`,
		installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (installation_id, type, payload) VALUES ($1, 'widget.ping', '{}'::jsonb)`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (installation_id, content_type, raw_body, body_sha256, parse_status)
		VALUES ($1, 'application/json', '{}', decode(repeat('aa', 32), 'hex'), 'parsed')`,
		installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, Command{Action: "uninstall", Actor: "test-operator", Code: "widget-a", InstallationID: installationID}); err != nil {
		t.Fatal(err)
	}
	var credentials, jobs, deliveries int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM oauth_credentials WHERE installation_id=$1),
		(SELECT count(*) FROM jobs WHERE installation_id=$1),
		(SELECT count(*) FROM webhook_deliveries WHERE installation_id=$1)`,
		installationID).Scan(&credentials, &jobs, &deliveries); err != nil {
		t.Fatal(err)
	}
	if credentials != 1 || jobs != 1 || deliveries != 1 {
		t.Fatalf("uninstall deleted owner data: credentials=%d jobs=%d deliveries=%d", credentials, jobs, deliveries)
	}
	var tokenLoad, jobAdmit, liveActive bool
	if err := pool.QueryRow(ctx, `
		SELECT
			EXISTS(
				SELECT 1 FROM oauth_credentials credentials
				JOIN installations installation ON installation.id = credentials.installation_id
				JOIN integrations integration ON integration.id = installation.integration_id
				WHERE credentials.installation_id = $1
				  AND installation.status IN ('active', 'authorizing')
				  AND integration.status = 'active'
			),
			EXISTS(
				SELECT 1 FROM installations AS installation
				JOIN integrations AS integration ON integration.id=installation.integration_id
				JOIN integration_services AS capability ON capability.integration_id=integration.id
				WHERE installation.id=$1 AND capability.service_code='lead-status'
				  AND installation.status='active' AND integration.status='active' AND capability.enabled
			),
			EXISTS(
				SELECT 1 FROM installations i
				JOIN integrations n ON n.id=i.integration_id
				WHERE i.id=$1 AND i.status='active' AND n.status='active'
			)`, installationID).Scan(&tokenLoad, &jobAdmit, &liveActive); err != nil {
		t.Fatal(err)
	}
	if tokenLoad || jobAdmit || liveActive {
		t.Fatalf("existing guards still admitted uninstalled installation: token=%v job=%v live=%v", tokenLoad, jobAdmit, liveActive)
	}
	if _, err := store.Apply(ctx, Command{Action: "disable-installation", Actor: "test-operator", Code: "widget-a", InstallationID: installationID}); err == nil {
		t.Fatal("disable after uninstall accepted")
	}
	if _, err := store.Apply(ctx, Command{Action: "enable-installation", Actor: "test-operator", Code: "widget-a", InstallationID: installationID}); err == nil {
		t.Fatal("enable after uninstall accepted")
	}
}

func TestInstallationDisableRevokeAndEnable(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	store, integrationID := newLifecycleStore(t, pool)
	installationID := insertLifecycleInstallation(t, pool, integrationID)
	disabled, err := store.Apply(ctx, Command{Action: "disable-installation", Actor: "test-operator", Code: "widget-a", InstallationID: installationID})
	if err != nil || disabled.Status != "disabled" {
		t.Fatalf("disable-installation: %#v %v", disabled, err)
	}
	if _, err := store.Apply(ctx, Command{Action: "revoke", Actor: "test-operator", Code: "widget-a", InstallationID: installationID}); err == nil {
		t.Fatal("revoke of disabled installation accepted")
	}
	enabled, err := store.Apply(ctx, Command{Action: "enable-installation", Actor: "test-operator", Code: "widget-a", InstallationID: installationID})
	if err != nil || enabled.Status != "active" {
		t.Fatalf("enable-installation: %#v %v", enabled, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO oauth_credentials (
			installation_id, access_token_ciphertext, refresh_token_ciphertext,
			expires_at, token_version, key_version
		) VALUES ($1, decode('00','hex'), decode('00','hex'), now() + interval '1 hour', 1, 1)`,
		installationID); err != nil {
		t.Fatal(err)
	}
	revoked, err := store.Apply(ctx, Command{Action: "revoke", Actor: "test-operator", Code: "widget-a", InstallationID: installationID})
	if err != nil || revoked.Status != "reauth_required" {
		t.Fatalf("revoke: %#v %v", revoked, err)
	}
	var credentials int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM oauth_credentials WHERE installation_id=$1`, installationID).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if credentials != 1 {
		t.Fatal("revoke deleted stored credentials")
	}
}

func newLifecycleStore(t *testing.T, pool *pgxpool.Pool) (*Store, uuid.UUID) {
	t.Helper()
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool, keys)
	uri := "https://example.test/oauth"
	created, err := store.Apply(context.Background(), Command{Action: "create", Actor: "test-operator", Code: "widget-a", ClientID: uuid.NewString(), Secret: []byte("synthetic-secret"), RedirectURI: &uri, Services: []string{"lead-status"}})
	if err != nil {
		t.Fatal(err)
	}
	return store, created.ID
}

func insertLifecycleInstallation(t *testing.T, pool *pgxpool.Pool, integrationID uuid.UUID) uuid.UUID {
	t.Helper()
	installationID := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO installations (id, integration_id, account_id, account_domain, status)
		VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active')`, installationID, integrationID); err != nil {
		t.Fatal(err)
	}
	return installationID
}
