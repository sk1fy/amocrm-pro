package adminread

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestAccountDiagnosticsConsistentAcrossListsAndCards(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := insertAdminIntegration(t, pool, "fixture-custom-widget")
	emptyIntegrationID := insertAdminIntegration(t, pool, "fixture-without-grants")
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services(integration_id, service_code, enabled)
		VALUES ($1, 'lead-status', true), ($1, 'activity', false)`, integrationID); err != nil {
		t.Fatal(err)
	}
	installationID, emptyID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	insertAdminInstallation(t, pool, installationID, integrationID, 91010101, "diagnostics.amocrm.test", "active", `{}`, now)
	insertAdminInstallation(t, pool, emptyID, emptyIntegrationID, 91010101, "diagnostics.amocrm.test", "active", `{}`, now)
	for _, job := range []struct {
		status string
		age    time.Duration
	}{
		{"failed", time.Hour}, {"dead", 2 * time.Hour},
		{"failed", 25 * time.Hour}, {"completed", time.Hour}, {"retry", time.Hour},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs(installation_id, type, status, payload, created_at, updated_at)
			VALUES ($1, 'widget.ping', $2, '{}', $3, $3)`, installationID, job.status, now.Add(-job.age)); err != nil {
			t.Fatal(err)
		}
	}
	wantGrants := []Grant{{Service: "activity", Enabled: false}, {Service: "lead-status", Enabled: true}}
	wantCounts := map[uuid.UUID]int{installationID: 2, emptyID: 0}
	router := adminTestRouter(t, pool)
	accounts := adminGET(t, router, "/admin/v1/accounts?q=91010101")
	assertAdminResponseSchema(t, accounts.Code, accounts.Body.Bytes(), "AccountListResponse")
	var accountList struct{ Items []AccountListItem }
	decodeJSON(t, accounts, &accountList)
	if len(accountList.Items) != 1 || len(accountList.Items[0].Installations) != 2 {
		t.Fatalf("unexpected account installations: %s", accounts.Body.String())
	}
	for _, item := range accountList.Items[0].Installations {
		if item.RecentFailedJobs != wantCounts[item.ID] {
			t.Fatalf("list failure count=%d for %s", item.RecentFailedJobs, item.ID)
		}
		if item.ID == installationID && !reflect.DeepEqual(item.Grants, wantGrants) {
			t.Fatalf("grants=%+v, want %+v", item.Grants, wantGrants)
		}
		if item.ID == emptyID && (item.Grants == nil || len(item.Grants) != 0) {
			t.Fatalf("missing grants must be [], got %+v", item.Grants)
		}
	}
	account := adminGET(t, router, "/admin/v1/accounts/91010101")
	assertAdminResponseSchema(t, account.Code, account.Body.Bytes(), "AccountResponse")
	var card AccountResponse
	decodeJSON(t, account, &card)
	for _, item := range card.Installations {
		if item.Installation.RecentFailedJobs != wantCounts[item.Installation.ID] {
			t.Fatalf("card failure count=%d for %s", item.Installation.RecentFailedJobs, item.Installation.ID)
		}
	}
	installations := adminGET(t, router, "/admin/v1/installations?account_id=91010101")
	assertAdminResponseSchema(t, installations.Code, installations.Body.Bytes(), "InstallationListResponse")
	var installationList struct{ Items []InstallationSummary }
	decodeJSON(t, installations, &installationList)
	for _, item := range installationList.Items {
		if item.RecentFailedJobs != wantCounts[item.ID] {
			t.Fatalf("installation list failure count=%d for %s", item.RecentFailedJobs, item.ID)
		}
	}
	installation := adminGET(t, router, "/admin/v1/installations/"+installationID.String())
	assertAdminResponseSchema(t, installation.Code, installation.Body.Bytes(), "InstallationResponse")
	var detail InstallationResponse
	decodeJSON(t, installation, &detail)
	if detail.Installation.RecentFailedJobs != 2 {
		t.Fatalf("installation card failure count=%d", detail.Installation.RecentFailedJobs)
	}
}

func TestAdminDeliveriesShowNewestWhileCLIPreservesOldestFirst(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := insertAdminIntegration(t, pool, "fixture-deliveries")
	installationID := uuid.New()
	insertAdminInstallation(t, pool, installationID, integrationID, 91020202, "deliveries.amocrm.test", "active", `{}`, time.Now().UTC())
	for range 11 {
		insertAdminDelivery(t, pool, installationID, integrationID)
	}
	var oldestID, newestID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT command_id FROM activity_command_receipts ORDER BY created_at, command_id LIMIT 1`).Scan(&oldestID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT command_id FROM activity_command_receipts ORDER BY created_at DESC, command_id DESC LIMIT 1`).Scan(&newestID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE activity_command_outbox SET status='failed' WHERE command_id IN ($1, $2)`, oldestID, newestID); err != nil {
		t.Fatal(err)
	}
	response := adminGET(t, adminTestRouter(t, pool), "/admin/v1/installations/"+installationID.String()+"/activity/deliveries?limit=10")
	assertAdminResponseSchema(t, response.Code, response.Body.Bytes(), "DeliveriesResponse")
	var deliveries DeliveriesResponse
	decodeJSON(t, response, &deliveries)
	if len(deliveries.Items) != 10 || deliveries.Items[0].CommandID != newestID || deliveries.Items[0].Status != "failed" {
		t.Fatalf("newest failed delivery must be first: %+v", deliveries.Items)
	}
	for _, item := range deliveries.Items {
		if item.CommandID == oldestID {
			t.Fatal("oldest delivery should fall outside the recent limit")
		}
	}
	cli, err := activitybridge.ListDeliveries(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(cli) != 2 || cli[0].CommandID != oldestID || cli[1].CommandID != newestID {
		t.Fatalf("CLI failed-only oldest-first ordering changed: %+v", cli)
	}
}

func assertAdminResponseSchema(t *testing.T, status int, raw []byte, name string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("response status=%d, body=%s", status, raw)
	}
	assertNoSecretJSONKeys(t, raw)
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromFile("../../api/admin-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := document.Components.Schemas[name].Value.VisitJSON(value); err != nil {
		t.Fatalf("%s contract: %v", name, err)
	}
}
