package adminread

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

const testAdminToken = "admin-dev-credential"

func TestAdminReadListsAndCards(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	router := adminTestRouter(t, pool)

	integrationID := insertAdminIntegration(t, pool, "widget-fixture-a")
	otherIntegrationID := insertAdminIntegration(t, pool, "widget-fixture-b")
	if _, err := pool.Exec(ctx, `INSERT INTO integration_services (integration_id, service_code, enabled)
		VALUES ($1, 'lead-status', true), ($1, 'activity', false)`, integrationID); err != nil {
		t.Fatal(err)
	}

	firstID := uuid.New()
	secondID := uuid.New()
	thirdID := uuid.New()
	insertAdminInstallation(t, pool, firstID, integrationID, 31415926, "fixture-alpha.amocrm.test", "active", `{"origin":"fixture"}`, time.Now().UTC().Add(-2*time.Hour))
	insertAdminInstallation(t, pool, secondID, otherIntegrationID, 31415926, "fixture-alpha.amocrm.test", "reauth_required", `{}`, time.Now().UTC().Add(-time.Hour))
	insertAdminInstallation(t, pool, thirdID, integrationID, 27182818, "fixture-beta.amocrm.test", "pending", `{}`, time.Now().UTC())
	insertAdminCredentials(t, pool, firstID, time.Now().UTC().Add(time.Hour))

	if _, err := pool.Exec(ctx, `INSERT INTO jobs (installation_id, type, status, payload, created_at, updated_at)
		VALUES ($1, 'widget.ping', 'queued', '{}'::jsonb, now(), now())`, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO jobs (installation_id, type, status, payload, created_at, updated_at)
		VALUES ($1, 'webhook.reconcile', 'failed', '{}'::jsonb, now(), now())`, firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_log (installation_id, actor_type, actor_id, action, object_type, object_id)
		VALUES ($1, 'operator', 'test', 'installation.view', 'installation', $2)`, firstID, firstID.String()); err != nil {
		t.Fatal(err)
	}
	insertAdminDelivery(t, pool, firstID, integrationID)

	list := adminGET(t, router, "/admin/v1/installations?limit=2")
	if list.Code != http.StatusOK {
		t.Fatalf("list installations=%d %s", list.Code, list.Body.String())
	}
	var listBody struct {
		Source     string `json:"source"`
		Items      []InstallationSummary
		NextCursor *string `json:"next_cursor"`
	}
	decodeJSON(t, list, &listBody)
	if listBody.Source != sourceCore || len(listBody.Items) != 2 || listBody.NextCursor == nil {
		t.Fatalf("list body=%s", list.Body.String())
	}
	assertNoSecretJSONKeys(t, list.Body.Bytes())

	page2 := adminGET(t, router, "/admin/v1/installations?limit=2&cursor="+*listBody.NextCursor)
	if page2.Code != http.StatusOK {
		t.Fatalf("page2=%d %s", page2.Code, page2.Body.String())
	}
	var page2Body struct {
		Items      []InstallationSummary
		NextCursor *string `json:"next_cursor"`
	}
	decodeJSON(t, page2, &page2Body)
	if len(page2Body.Items) != 1 {
		t.Fatalf("page2 items=%d %s", len(page2Body.Items), page2.Body.String())
	}
	repeat := adminGET(t, router, "/admin/v1/installations?limit=2&cursor="+*listBody.NextCursor)
	var repeatBody struct {
		Items []InstallationSummary
	}
	decodeJSON(t, repeat, &repeatBody)
	if len(repeatBody.Items) != 1 || repeatBody.Items[0].ID != page2Body.Items[0].ID {
		t.Fatal("cursor is not stable")
	}

	clamped := adminGET(t, router, "/admin/v1/installations?limit=101")
	if clamped.Code != http.StatusOK {
		t.Fatalf("clamped=%d %s", clamped.Code, clamped.Body.String())
	}

	accounts := adminGET(t, router, "/admin/v1/accounts?q=fixture-alpha.amocrm.test")
	if accounts.Code != http.StatusOK {
		t.Fatalf("accounts=%d %s", accounts.Code, accounts.Body.String())
	}
	var accountList struct {
		Items []AccountListItem
		Total *int64 `json:"total"`
	}
	decodeJSON(t, accounts, &accountList)
	if len(accountList.Items) != 1 || accountList.Items[0].AccountID != 31415926 || accountList.Items[0].Origin != originFixture {
		t.Fatalf("accounts=%s", accounts.Body.String())
	}
	if accountList.Total == nil || *accountList.Total != 1 {
		t.Fatalf("accounts total=%v body=%s", accountList.Total, accounts.Body.String())
	}
	installations := accountList.Items[0].Installations
	if len(installations) != 2 {
		t.Fatalf("account installations=%d", len(installations))
	}
	authByID := map[uuid.UUID]string{}
	failedByID := map[uuid.UUID]int{}
	for _, item := range installations {
		authByID[item.ID] = item.Authorization
		failedByID[item.ID] = item.RecentFailedJobs
	}
	if authByID[firstID] != authValid || authByID[secondID] != authMissing {
		t.Fatalf("list authorization=%v body=%s", authByID, accounts.Body.String())
	}
	if failedByID[firstID] != 1 {
		t.Fatalf("recent failed jobs=%v body=%s", failedByID, accounts.Body.String())
	}
	assertNoSecretJSONKeys(t, accounts.Body.Bytes())

	allAccounts := adminGET(t, router, "/admin/v1/accounts?limit=1")
	var allAccountsBody struct {
		Items []AccountListItem
		Total *int64 `json:"total"`
	}
	decodeJSON(t, allAccounts, &allAccountsBody)
	if len(allAccountsBody.Items) != 1 || allAccountsBody.Total == nil || *allAccountsBody.Total != 2 {
		t.Fatalf("accounts total with limit=1: %s", allAccounts.Body.String())
	}

	account := adminGET(t, router, "/admin/v1/accounts/31415926")
	if account.Code != http.StatusOK {
		t.Fatalf("account=%d %s", account.Code, account.Body.String())
	}
	var accountBody AccountResponse
	decodeJSON(t, account, &accountBody)
	if len(accountBody.Installations) != 2 {
		t.Fatalf("account installations=%d %s", len(accountBody.Installations), account.Body.String())
	}
	assertNoSecretJSONKeys(t, account.Body.Bytes())

	missingAuth := adminGET(t, router, "/admin/v1/installations/"+secondID.String())
	if missingAuth.Code != http.StatusOK {
		t.Fatalf("card=%d %s", missingAuth.Code, missingAuth.Body.String())
	}
	var card InstallationResponse
	decodeJSON(t, missingAuth, &card)
	if card.Authorization.State != authMissing || card.Authorization.CredentialsPresent || !card.Authorization.Unverified {
		t.Fatalf("authorization=%+v", card.Authorization)
	}
	if card.Installation.Origin != originReal || card.Activity.Pilot != pilotNotConfigured {
		t.Fatalf("card=%s", missingAuth.Body.String())
	}
	assertNoSecretJSONKeys(t, missingAuth.Body.Bytes())

	present := adminGET(t, router, "/admin/v1/installations/"+firstID.String())
	var presentCard InstallationResponse
	decodeJSON(t, present, &presentCard)
	if presentCard.Authorization.State != authValid || !presentCard.Authorization.CredentialsPresent || presentCard.Authorization.CredentialVersion != 3 {
		t.Fatalf("valid card=%s", present.Body.String())
	}
	if presentCard.Installation.Origin != originFixture {
		t.Fatalf("origin=%s", presentCard.Installation.Origin)
	}
	assertNoSecretJSONKeys(t, present.Body.Bytes())

	backend := adminGET(t, router, "/admin/v1/backend")
	if backend.Code != http.StatusOK {
		t.Fatalf("backend=%d %s", backend.Code, backend.Body.String())
	}
	assertNoSecretJSONKeys(t, backend.Body.Bytes())

	jobs := adminGET(t, router, "/admin/v1/jobs")
	if jobs.Code != http.StatusOK {
		t.Fatalf("jobs=%d %s", jobs.Code, jobs.Body.String())
	}
	var jobsBody struct {
		Items []Job
	}
	decodeJSON(t, jobs, &jobsBody)
	if len(jobsBody.Items) != 2 {
		t.Fatalf("jobs items=%d body=%s", len(jobsBody.Items), jobs.Body.String())
	}
	if jobsBody.Items[0].AccountID == nil || *jobsBody.Items[0].AccountID != 31415926 {
		t.Fatalf("job account_id=%v body=%s", jobsBody.Items[0].AccountID, jobs.Body.String())
	}
	assertNoSecretJSONKeys(t, jobs.Body.Bytes())

	job := adminGET(t, router, "/admin/v1/jobs/"+jobsBody.Items[0].ID.String())
	if job.Code != http.StatusOK {
		t.Fatalf("job=%d %s", job.Code, job.Body.String())
	}
	assertNoSecretJSONKeys(t, job.Body.Bytes())

	integrations := adminGET(t, router, "/admin/v1/integrations")
	if integrations.Code != http.StatusOK {
		t.Fatalf("integrations=%d %s", integrations.Code, integrations.Body.String())
	}
	assertNoSecretJSONKeys(t, integrations.Body.Bytes())

	integration := adminGET(t, router, "/admin/v1/integrations/"+integrationID.String())
	if integration.Code != http.StatusOK {
		t.Fatalf("integration=%d %s", integration.Code, integration.Body.String())
	}
	assertNoSecretJSONKeys(t, integration.Body.Bytes())

	audit := adminGET(t, router, "/admin/v1/audit?installation_id="+firstID.String())
	if audit.Code != http.StatusOK {
		t.Fatalf("audit=%d %s", audit.Code, audit.Body.String())
	}
	var auditBody struct {
		Items []AuditEntry
	}
	decodeJSON(t, audit, &auditBody)
	if len(auditBody.Items) != 1 {
		t.Fatalf("audit items=%d body=%s", len(auditBody.Items), audit.Body.String())
	}
	assertNoSecretJSONKeys(t, audit.Body.Bytes())

	installationAudit := adminGET(t, router, "/admin/v1/installations/"+firstID.String()+"/audit")
	if installationAudit.Code != http.StatusOK {
		t.Fatalf("installation audit=%d %s", installationAudit.Code, installationAudit.Body.String())
	}
	assertNoSecretJSONKeys(t, installationAudit.Body.Bytes())

	deliveries := adminGET(t, router, "/admin/v1/installations/"+firstID.String()+"/activity/deliveries")
	if deliveries.Code != http.StatusOK {
		t.Fatalf("deliveries=%d %s", deliveries.Code, deliveries.Body.String())
	}
	var deliveryBody struct {
		Items []any
	}
	decodeJSON(t, deliveries, &deliveryBody)
	if len(deliveryBody.Items) != 1 {
		t.Fatalf("deliveries=%s", deliveries.Body.String())
	}
	assertNoSecretJSONKeys(t, deliveries.Body.Bytes())

	if rec := adminGET(t, router, "/admin/v1/jobs?since="+time.Now().UTC().Add(-8*24*time.Hour).Format(time.RFC3339)); rec.Code != http.StatusBadRequest {
		t.Fatalf("jobs window=%d %s", rec.Code, rec.Body.String())
	}

	unauth := httptest.NewRequest(http.MethodGet, "/admin/v1/backend", nil)
	unauthRec := httptest.NewRecorder()
	router.ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth=%d %s", unauthRec.Code, unauthRec.Body.String())
	}
}

func adminTestRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	Register(router, Dependencies{
		Pool: pool, Timeout: 2 * time.Second, Token: testAdminToken, Revision: "test",
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	return router
}

func adminGET(t *testing.T, router http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	req.Header.Set("X-Admin-Actor", "employee:11111111-1111-1111-1111-111111111111")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dest any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dest); err != nil {
		t.Fatalf("decode %v body=%s", err, rec.Body.String())
	}
}

func insertAdminIntegration(t *testing.T, pool *pgxpool.Pool, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO integrations (id, code, client_id, client_secret_ciphertext, redirect_uri, status)
		VALUES ($1, $2, $3, $4, $5, 'active')`,
		id, code, uuid.NewString(), []byte("fixture"), "https://backend.example.test/oauth/amocrm/callback")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertAdminInstallation(
	t *testing.T,
	pool *pgxpool.Pool,
	id, integrationID uuid.UUID,
	accountID int64,
	domain, status, settings string,
	updated time.Time,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO installations (id, integration_id, account_id, account_domain, status, settings, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $7)`,
		id, integrationID, accountID, domain, status, settings, updated)
	if err != nil {
		t.Fatal(err)
	}
}

func insertAdminCredentials(t *testing.T, pool *pgxpool.Pool, installationID uuid.UUID, expires time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO oauth_credentials (
			installation_id, access_token_ciphertext, refresh_token_ciphertext,
			expires_at, token_version, key_version
		) VALUES ($1, $2, $2, $3, 3, 1)`, installationID, []byte("fixture"), expires)
	if err != nil {
		t.Fatal(err)
	}
}

func insertAdminDelivery(t *testing.T, pool *pgxpool.Pool, installationID, integrationID uuid.UUID) {
	t.Helper()
	commandID := uuid.New()
	key := sha256.Sum256([]byte(commandID.String()))
	request := sha256.Sum256([]byte("request:" + commandID.String()))
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO activity_command_receipts(
			command_id, installation_id, integration_id, actor_id, target, action, key_hash, request_hash
		) VALUES ($1, $2, $3, 7, 'activity', 'settings', $4, $5)`,
		commandID, installationID, integrationID, key[:], request[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO activity_command_outbox(command_id, payload, status)
		VALUES ($1, '{}', 'accepted')`, commandID); err != nil {
		t.Fatal(err)
	}
}

func assertNoSecretJSONKeys(t *testing.T, raw []byte) {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	assertNoSecretKeys(t, value)
}

func assertNoSecretKeys(t *testing.T, value any) {
	t.Helper()
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			lower := strings.ToLower(key)
			for _, forbidden := range []string{"secret", "token", "ciphertext", "key_hash", "password"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("forbidden JSON key %q", key)
				}
			}
			assertNoSecretKeys(t, child)
		}
	case []any:
		for _, child := range node {
			assertNoSecretKeys(t, child)
		}
	}
}
