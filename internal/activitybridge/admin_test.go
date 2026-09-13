package activitybridge

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestAdmitAdminSkipsWidgetTokensAndAuditsAdmin(t *testing.T) {
	pool, _ := bridgeDatabase(t)
	ctx := context.Background()
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri) VALUES($1,$2,$3,$4,'https://example.test/callback')`, scope.IntegrationID, "admin-"+scope.IntegrationID.String(), uuid.NewString(), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,99,'tenant.amocrm.ru','active')`, scope.InstallationID, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled) VALUES($1,'activity',true)`, scope.IntegrationID); err != nil {
		t.Fatal(err)
	}
	b := New(pool, &admissionPolicy{}, &acceptingActivity{}, nil)
	if err := SetPilot(ctx, pool, scope.InstallationID, true); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"initial_days":2,"retention_days":7}`)
	receipt, err := b.AdmitAdmin(ctx, scope, "employee:fixture", "admin-key", serviceapi.ActivityService, serviceapi.ActionSettings, payload, payload)
	if err != nil || receipt.CommandID == "" || receipt.State != "pending_delivery" {
		t.Fatalf("admit=%+v %v", receipt, err)
	}
	var tokens, actor int64
	var actorType string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM used_widget_tokens),(SELECT actor_id FROM activity_command_receipts WHERE command_id=$1)`, receipt.CommandID).Scan(&tokens, &actor); err != nil || tokens != 0 || actor != 0 {
		t.Fatalf("tokens=%d actor=%d err=%v", tokens, actor, err)
	}
	if err := pool.QueryRow(ctx, `SELECT actor_type FROM audit_log WHERE action='activity.command.accepted' AND object_id=$1`, receipt.CommandID).Scan(&actorType); err != nil || actorType != "admin" {
		t.Fatalf("audit actor=%s %v", actorType, err)
	}
	replay, err := b.AdmitAdmin(ctx, scope, "employee:fixture", "admin-key", serviceapi.ActivityService, serviceapi.ActionSettings, payload, payload)
	if err != nil || replay.CommandID != receipt.CommandID || !replay.Replayed {
		t.Fatalf("replay=%+v %v", replay, err)
	}
}

func TestAdminIssueUnavailableWithoutPolicy(t *testing.T) {
	var b *Bridge
	if _, err := b.AdminIssue(context.Background(), serviceapi.Scope{}, "req", serviceapi.ActivityService, serviceapi.ActionSettings); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("nil bridge=%v", err)
	}
	b = New(nil, nil, nil, nil)
	if _, err := b.AdminSettings(context.Background(), serviceapi.Scope{}, "req"); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("nil activity=%v", err)
	}
}
