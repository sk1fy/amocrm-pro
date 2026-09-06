package corepolicy

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
)

type roleReader struct {
	admin, active bool
	calls         int
}

func (r *roleReader) GetUserAuthorization(_ context.Context, _ uuid.UUID, id int64) (amocrm.UserAuthorization, error) {
	r.calls++
	u := amocrm.UserAuthorization{ID: id}
	u.Rights.IsActive = r.active
	u.Rights.IsAdmin = r.admin
	return u, nil
}
func TestDatabasePolicyChecksCurrentPilotCapabilityRoleAndInstallation(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri)VALUES($1,$2,$2,'secret','https://example.test/oauth')`, scope.IntegrationID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'example.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled)VALUES($1,'activity',true)`, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	reader := &roleReader{admin: true, active: true}
	check := databaseChecker{pool, reader}
	if err := check.Check(ctx, scope, 7, false); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("default-off pilot: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO activity_pilots(installation_id,enabled)VALUES($1,true)`, scope.InstallationID); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(ctx, scope, 7, false); err != nil {
		t.Fatal(err)
	}
	reader.admin = false
	if err := check.Check(ctx, scope, 7, false); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("current nonadmin accepted %v", err)
	}
	reader.admin = true
	reader.active = false
	if err := check.Check(ctx, scope, 7, false); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("inactive admin accepted %v", err)
	}
	reader.active = true
	other := scope
	other.IntegrationID = uuid.New()
	if err := check.Check(ctx, other, 7, false); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("wrong integration accepted %v", err)
	}
	for _, query := range []string{`UPDATE activity_pilots SET enabled=false WHERE installation_id=$1`, `UPDATE integration_services SET enabled=false WHERE integration_id=(SELECT integration_id FROM installations WHERE id=$1)`, `UPDATE installations SET status='disabled' WHERE id=$1`} {
		if _, err := pool.Exec(ctx, query, scope.InstallationID); err != nil {
			t.Fatal(err)
		}
		calls := reader.calls
		if err := check.Check(ctx, scope, 0, true); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("background revocation %v", err)
		}
		if reader.calls != calls {
			t.Fatal("system policy unexpectedly fetched user role")
		}
		if _, err := pool.Exec(ctx, `UPDATE activity_pilots SET enabled=true WHERE installation_id=$1`, scope.InstallationID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE integration_services SET enabled=true WHERE integration_id=$1`, scope.IntegrationID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE installations SET status='active' WHERE id=$1`, scope.InstallationID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET status='reauth_required' WHERE id=$1`, scope.InstallationID); err != nil {
		t.Fatal(err)
	}
	if err := check.Check(ctx, scope, 0, true); serviceapi.ErrorCode(err) != serviceapi.ReauthRequired {
		t.Fatalf("reauth state %v", err)
	}
}
