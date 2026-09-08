package componentruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platform/migrations"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

// These are real executable/process tests with a synthetic amoCRM boundary.
// They intentionally require an explicitly authorized disposable cluster.
func TestComponentProcessesAndModeSwitch(t *testing.T) {
	adminURL := os.Getenv("COMPONENT_PROCESS_TEST_ADMIN_URL")
	if adminURL == "" || os.Getenv("COMPONENT_PROCESS_TEST_ALLOWED") != "true" {
		t.Skip("set COMPONENT_PROCESS_TEST_ADMIN_URL and COMPONENT_PROCESS_TEST_ALLOWED=true for disposable process topology")
	}

	previousAPI := strings.TrimSpace(os.Getenv("COMPONENT_PROCESS_PREVIOUS_API_BINARY"))
	if previousAPI == "" {
		t.Log("VERSION-SKEW SKIPPED: optional COMPONENT_PROCESS_PREVIOUS_API_BINARY unset; grpc phases use current API")
	} else {
		if !filepath.IsAbs(previousAPI) {
			t.Fatal("COMPONENT_PROCESS_PREVIOUS_API_BINARY must be an absolute path")
		}
		info, err := os.Stat(previousAPI)
		mustProcess(t, err)
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			t.Fatal("previous API must be an executable regular file")
		}
		metadata, err := buildinfo.ReadFile(previousAPI)
		mustProcess(t, err)
		if metadata.Path != "github.com/sk1fy/amocrm-pro/cmd/api" {
			t.Fatalf("previous binary has wrong Go target %q", metadata.Path)
		}
		data, err := os.ReadFile(previousAPI)
		mustProcess(t, err)
		sum := sha256.Sum256(data)
		t.Logf("VERSION-SKEW ENABLED: grpc Core uses preserved API %s SHA256=%x Go=%s; embedded Core/Activity/CRM Events are current race builds; previous production API may be nonrace; current Gateway fixture", previousAPI, sum, metadata.GoVersion)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root, err := filepath.Abs("../..")
	mustProcess(t, err)
	temp := t.TempDir()
	bins := filepath.Join(temp, "bin")
	mustProcess(t, os.Mkdir(bins, 0700))
	for _, name := range []string{"api", "activity", "crm-events", "service-certs"} {
		// The bind-mounted CI checkout can have a different owner from this
		// container. Test executables need no VCS stamp (and no Git trust override).
		cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-race", "-o", filepath.Join(bins, name), "./cmd/"+name)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", name, err, output)
		}
	}
	identities := filepath.Join(temp, "identity")
	cmd := exec.CommandContext(ctx, filepath.Join(bins, "service-certs"), identities)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate certs: %v %s", err, output)
	}
	admin, err := pgxpool.New(ctx, adminURL)
	mustProcess(t, err)
	defer admin.Close()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	dbs := map[string]string{}
	owners := map[string]*pgxpool.Pool{}
	for _, role := range []string{"core", "activity", "events"} {
		db := "activity_process_" + suffix + "_" + role + "_test"
		dbs[role] = db
		quoted := pgx.Identifier{db}.Sanitize()
		_, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER "+role+"_owner")
		mustProcess(t, err)
		t.Cleanup(func() {
			cleanupCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			cleanup, err := pgxpool.New(cleanupCtx, adminURL)
			if err == nil {
				defer cleanup.Close()
				_, _ = cleanup.Exec(cleanupCtx, "DROP DATABASE "+quoted+" WITH (FORCE)")
			}
		})
		_, err = admin.Exec(ctx, "REVOKE ALL ON DATABASE "+quoted+" FROM PUBLIC")
		mustProcess(t, err)
		_, err = admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+quoted+" TO "+role+"_runtime")
		mustProcess(t, err)
		owner, err := pgxpool.New(ctx, processDSN(t, adminURL, db, role+"_owner"))
		mustProcess(t, err)
		owners[role] = owner
		t.Cleanup(owner.Close)
		dir := filepath.Join(root, "migrations")
		if role == "activity" {
			dir = filepath.Join(dir, "activity")
		}
		if role == "events" {
			dir = filepath.Join(dir, "crmevents")
		}
		mustProcess(t, migrations.New(owner, dir).Up(ctx))
		_, err = owner.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA public TO "+role+"_runtime; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO "+role+"_runtime; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO "+role+"_runtime")
		mustProcess(t, err)
	}
	// Each runtime role is actually denied CONNECT to both foreign databases.
	for _, role := range []string{"core", "activity", "events"} {
		for other, db := range dbs {
			if role == other {
				continue
			}
			probe, err := pgxpool.New(ctx, processDSN(t, adminURL, db, role+"_runtime"))
			mustProcess(t, err)
			err = probe.Ping(ctx)
			probe.Close()
			if err == nil {
				t.Fatalf("%s accessed foreign %s database", role, other)
			}
		}
	}
	key := bytes.Repeat([]byte{0x31}, 32)
	encodedKey := "1:" + base64.StdEncoding.EncodeToString(key)
	ring, err := cryptox.NewKeyRing(map[int][]byte{1: key}, 1)
	mustProcess(t, err)
	activityTenant := seedProcessTenant(t, ctx, owners["core"], ring, true)
	secondTenant := seedProcessTenant(t, ctx, owners["core"], ring, false)
	coreDSN := processDSN(t, adminURL, dbs["core"], "core_runtime")
	activityDSN := processDSN(t, adminURL, dbs["activity"], "activity_runtime")
	eventsDSN := processDSN(t, adminURL, dbs["events"], "events_runtime")
	gatewayAddr := processAddress(t)
	activityAddr := processAddress(t)
	eventsAddr := processAddress(t)
	apiAddr := processAddress(t)
	managementAddr := processAddress(t)
	activityHealth := processAddress(t)
	eventsHealth := processAddress(t)
	common := map[string]string{"ACTIVITY_MODE": "grpc", "GATEWAY_ADDRESS": gatewayAddr, "ACTIVITY_ADDRESS": activityAddr, "CRM_EVENTS_ADDRESS": eventsAddr, "CRM_EVENTS_WORKERS": "2", "CRM_EVENTS_POLL_INTERVAL": "1s", "CRM_EVENTS_ENRICHMENT": "0", "LOG_LEVEL": "warn"}
	tenants, _ := json.Marshal(map[string]string{activityTenant.InstallationID.String(): activityTenant.IntegrationID.String(), secondTenant.InstallationID.String(): secondTenant.IntegrationID.String()})
	eventAt := time.Now().Add(-2 * time.Minute).Unix()
	fixtureLog := filepath.Join(temp, "amo-requests.jsonl")
	eventCountFile := filepath.Join(temp, "event-count")
	mustProcess(t, os.WriteFile(eventCountFile, []byte("1"), 0600))
	fixtureMetrics := processAddress(t)
	exe, err := os.Executable()
	mustProcess(t, err)
	fixtureEnv := mergeProcessEnv(common, map[string]string{"COMPONENT_GATEWAY_FIXTURE": "1", "DATABASE_URL": coreDSN, "SERVICE_IDENTITY_DIR": filepath.Join(identities, "gateway"), "SERVICE_RPC_ADDRESS": gatewayAddr, "FIXTURE_TENANTS": string(tenants), "FIXTURE_EVENT_AT": strconv.FormatInt(eventAt, 10), "FIXTURE_REQUEST_LOG": fixtureLog, "FIXTURE_EVENT_COUNT_FILE": eventCountFile, "FIXTURE_METRICS_ADDRESS": fixtureMetrics})
	fixture := startProcess(t, temp, "gateway", exe, []string{"-test.run=^TestProcessGatewayFixture$", "-test.v"}, fixtureEnv)
	defer fixture.stop()
	waitProcess(t, ctx, 10*time.Second, func() bool {
		tls, err := servicerpc.TLSFromFiles(filepath.Join(identities, "core", "ca.crt"), filepath.Join(identities, "core", "tls.crt"), filepath.Join(identities, "core", "tls.key"), "localhost", false)
		if err != nil {
			return false
		}
		c, err := servicerpc.Dial(ctx, gatewayAddr, tls)
		if err != nil {
			return false
		}
		defer c.Close()
		return c.Ready(ctx) == nil
	})

	// OAuth account discovery crosses the actual Core-to-Gateway process boundary,
	// including an integration sharing this account, before product calls start.
	for _, identity := range []string{"core", "activity", "crm-events"} {
		tls, err := servicerpc.TLSFromFiles(filepath.Join(identities, identity, "ca.crt"), filepath.Join(identities, identity, "tls.crt"), filepath.Join(identities, identity, "tls.key"), "localhost", false)
		mustProcess(t, err)
		bootstrap, err := servicerpc.Dial(ctx, gatewayAddr, tls)
		mustProcess(t, err)
		for _, tenant := range []processTenant{activityTenant, secondTenant} {
			result, callErr := bootstrap.BootstrapAccount.GetAccount(ctx, serviceapi.BootstrapAccountRequest{IntegrationID: tenant.IntegrationID, AccountDomain: "fixture.amocrm.ru", AccessToken: tenant.IntegrationID.String()})
			if identity == "core" {
				if callErr != nil || result.ID != 42 || result.Subdomain != "fixture" {
					t.Fatalf("process Core OAuth bootstrap=%+v err=%v", result, callErr)
				}
			} else if serviceapi.ErrorCode(callErr) != serviceapi.PermissionDenied {
				t.Fatalf("product %s accessed OAuth bootstrap: %v", identity, callErr)
			}
		}
		mustProcess(t, bootstrap.Close())
	}
	t.Log("BOOTSTRAP verified: Core-only mTLS RPC reads account through the shared client for both same-account integrations; product identities denied")

	apiEnv := func(mode string) map[string]string {
		e := mergeProcessEnv(common, map[string]string{"ACTIVITY_MODE": mode, "DATABASE_URL": coreDSN, "ENCRYPTION_KEYS": encodedKey, "SERVICE_IDENTITY_DIR": filepath.Join(identities, "core"), "HTTP_ADDRESS": apiAddr, "MANAGEMENT_HTTP_ADDRESS": managementAddr, "DB_MAX_CONNS": "5", "WIDGET_INSTALLATION_RATE_PER_SECOND": "1000", "WIDGET_INSTALLATION_BURST": "1000", "WIDGET_INTEGRATION_RATE_PER_SECOND": "1000", "WIDGET_INTEGRATION_BURST": "1000"})
		if mode == "embedded" {
			e["EMBEDDED_IDENTITY_DIR"] = identities
			e["ACTIVITY_DATABASE_URL"] = activityDSN
			e["CRM_EVENTS_DATABASE_URL"] = eventsDSN
		}
		return e
	}
	runAPI := func(mode string) *processChild {
		binary := filepath.Join(bins, "api")
		if mode == "grpc" && previousAPI != "" {
			binary = previousAPI
			t.Log("VERSION-SKEW starting preserved Core API with current Activity and CRM Events")
		}
		p := startProcess(t, temp, "api-"+mode, binary, nil, apiEnv(mode))
		waitHTTPProcess(t, ctx, "http://"+apiAddr+"/live")
		return p
	}
	api := runAPI("embedded")
	defer func() { api.stop() }()
	client := processWidgetClient{base: "http://" + apiAddr, tenant: activityTenant, t: t}
	settingsBody := `{"initial_days":1,"retention_days":7}`
	receipt := client.accept(ctx, "/api/v1/widget/activity/settings", "settings-stable", settingsBody)
	client.waitOperation(ctx, receipt.CommandID)
	syncReceipt := client.accept(ctx, "/api/v1/widget/activity/sync", "enable-stable", `{"kind":"enable"}`)
	// Baseline existing widget uses its own installation and queue concurrently.
	second := processWidgetClient{base: client.base, tenant: secondTenant, t: t}
	started := time.Now()
	status, body := second.request(ctx, "POST", "/api/v1/widget/actions/leads/set-status", "second-widget", `{"lead_id":42,"pipeline_id":10,"status_id":30}`)
	if status != 202 {
		t.Fatalf("second widget status=%d %s", status, body)
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	mustProcess(t, json.Unmarshal(body, &accepted))
	waitProcess(t, ctx, 20*time.Second, func() bool {
		status, data := second.request(ctx, "GET", "/api/v1/widget/jobs/"+accepted.JobID, "", "")
		return status == 200 && bytes.Contains(data, []byte(`"status":"completed"`))
	})
	t.Logf("second existing leadstatus job completed during collection in %s", time.Since(started))
	client.waitOperation(ctx, syncReceipt.CommandID)
	panel := client.panel(ctx, eventAt-60, eventAt+60)
	if len(panel.Data.Events) != 1 || panel.Data.Summaries[0].UniqueEvents != 1 {
		t.Fatalf("embedded useful panel=%+v", panel)
	}
	// Quiesce embedded scheduler by shutting down its owning API, preserving DBs.
	api.stop()
	runEvents := func() *processChild {
		e := mergeProcessEnv(common, map[string]string{"SERVICE_IDENTITY_DIR": filepath.Join(identities, "crm-events"), "SERVICE_RPC_ADDRESS": eventsAddr, "SERVICE_HEALTH_ADDRESS": eventsHealth, "CRM_EVENTS_DATABASE_URL": eventsDSN})
		p := startProcess(t, temp, "crm-events", filepath.Join(bins, "crm-events"), nil, e)
		waitHTTPProcess(t, ctx, "http://"+eventsHealth+"/ready")
		return p
	}
	runActivity := func() *processChild {
		e := mergeProcessEnv(common, map[string]string{"SERVICE_IDENTITY_DIR": filepath.Join(identities, "activity"), "SERVICE_RPC_ADDRESS": activityAddr, "SERVICE_HEALTH_ADDRESS": activityHealth, "ACTIVITY_DATABASE_URL": activityDSN})
		p := startProcess(t, temp, "activity", filepath.Join(bins, "activity"), nil, e)
		waitHTTPProcess(t, ctx, "http://"+activityHealth+"/ready")
		return p
	}
	eventsProcess := runEvents()
	defer func() { eventsProcess.stop() }()
	productProcess := runActivity()
	defer func() { productProcess.stop() }()
	api = runAPI("grpc")
	panel = client.panel(ctx, eventAt-60, eventAt+60)
	if len(panel.Data.Events) != 1 || panel.Data.Events[0].ID != "fixture-event" {
		t.Fatalf("grpc retained panel=%+v", panel)
	}
	replay := client.accept(ctx, "/api/v1/widget/activity/settings", "settings-stable", settingsBody)
	if replay.CommandID != receipt.CommandID {
		t.Fatal("mode switch changed command identity")
	}
	client.waitOperation(ctx, replay.CommandID)
	// Core starts and remains useful while optional Activity is stopped.
	productProcess.stop()
	api.stop()
	api = runAPI("grpc")
	waitHTTPProcess(t, ctx, "http://"+managementAddr+"/ready")
	status, body = second.request(ctx, "POST", "/api/v1/widget/actions/ping", "core-restart-product-down", "")
	if status != 202 {
		t.Fatalf("Core restart with Activity down broke widget ping=%d %s", status, body)
	}
	var downPing struct {
		JobID string `json:"job_id"`
	}
	mustProcess(t, json.Unmarshal(body, &downPing))
	waitProcess(t, ctx, 10*time.Second, func() bool {
		status, data := second.request(ctx, "GET", "/api/v1/widget/jobs/"+downPing.JobID, "", "")
		return status == 200 && bytes.Contains(data, []byte(`"status":"completed"`))
	})
	// The newly started Core accepts durably and restores owner delivery later.
	pending := client.accept(ctx, "/api/v1/widget/activity/settings", "recipient-down", `{"initial_days":1,"retention_days":8}`)
	if pending.State != "pending_delivery" {
		t.Fatalf("outage acceptance=%+v", pending)
	}
	var count int
	mustProcess(t, owners["core"].QueryRow(ctx, `SELECT count(*) FROM activity_command_outbox WHERE command_id=$1`, pending.CommandID).Scan(&count))
	if count != 1 {
		t.Fatal("202 lacked durable outbox")
	}
	productProcess = runActivity()
	client.waitOperation(ctx, pending.CommandID)
	status, body = second.request(ctx, "GET", "/api/v1/widget/bootstrap", "", "")
	if status != 200 {
		t.Fatalf("second widget after independent restart=%d %s", status, body)
	}

	// Prespecified bounded impact workload: 50 ping admissions, concurrency2,
	// baseline collector disabled then10000 events in stable100-row pages/2slots.
	// Thresholds recorded before execution: zero errors, loadedP95<1s,P99<2s,
	// loadedP95<=max(3*idleP95,100ms). Additionally each phase runs30 real
	// leadstatus mutations using2closed-loop callers and distinct lead IDs:
	// no errors/retries, admissionP95<1s/P99<2s, durablecompletionP95<3s/P99<5s,
	// loadedcompletionP95<=max(3*idlecompletionP95,1s). The10000event backfill
	// must remain active through every loaded product job. Fixture limits only.
	disabled := client.accept(ctx, "/api/v1/widget/activity/sync", "impact-pause", `{"kind":"disable"}`)
	client.waitOperation(ctx, disabled.CommandID)
	idle := processPingWorkload(t, ctx, second, owners["core"], "idle")
	idleProduct := processProductWorkload(t, ctx, second, owners["core"], nil, "idle", 10000)
	enabled := client.accept(ctx, "/api/v1/widget/activity/sync", "impact-resume", `{"kind":"enable"}`)
	client.waitOperation(ctx, enabled.CommandID)
	mustProcess(t, os.WriteFile(eventCountFile, []byte("10000"), 0600))
	backfill := client.accept(ctx, "/api/v1/widget/activity/sync", "impact-backfill", fmt.Sprintf(`{"kind":"backfill","from":%d,"to":%d}`, eventAt-60, eventAt+60))
	waitProcess(t, ctx, 15*time.Second, func() bool {
		var count int
		err := owners["events"].QueryRow(ctx, `SELECT count(*) FROM event_jobs WHERE kind='backfill' AND status='running'`).Scan(&count)
		return err == nil && count > 0
	})
	metricBefore := processMetrics(t, ctx, "http://"+managementAddr+"/metrics")
	loaded := processPingWorkload(t, ctx, second, owners["core"], "loaded")
	loadedProduct := processProductWorkload(t, ctx, second, owners["core"], owners["events"], "loaded", 20000)
	if loadedProduct.completion.p95 > maxDuration(3*idleProduct.completion.p95, time.Second) {
		t.Fatalf("product completion impact exceeded ratio: idle=%+v loaded=%+v", idleProduct, loadedProduct)
	}
	t.Logf("PRODUCT IMPACT passed:30real leadstatus mutations/phase,2closed-loop callers,10000event backfill active throughout; idle admissionP95=%s P99=%s completionP95=%s P99=%s; loaded admissionP95=%s P99=%s completionP95=%s P99=%s; errors=0,retries=0", idleProduct.admission.p95, idleProduct.admission.p99, idleProduct.completion.p95, idleProduct.completion.p99, loadedProduct.admission.p95, loadedProduct.admission.p99, loadedProduct.completion.p95, loadedProduct.completion.p99)
	metricAfter := processMetrics(t, ctx, "http://"+managementAddr+"/metrics")
	if loaded.p95 >= time.Second || loaded.p99 >= 2*time.Second || loaded.p95 > maxDuration(3*idle.p95, 100*time.Millisecond) {
		t.Fatalf("impact threshold failed idle=%+v loaded=%+v", idle, loaded)
	}
	metrics := map[string]string{"core_before": metricBefore, "core_after": metricAfter, "events": processMetrics(t, ctx, "http://"+eventsHealth+"/metrics"), "gateway": processMetrics(t, ctx, "http://"+fixtureMetrics+"/metrics")}
	for owner, body := range metrics {
		var keep []string
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "#") && (strings.HasPrefix(line, "component_db_") || strings.HasPrefix(line, "crm_events_") || strings.HasPrefix(line, "amocrm_budget_wait_seconds_sum") || strings.HasPrefix(line, "amocrm_requests_total")) {
				keep = append(keep, line)
			}
		}
		t.Logf("impact %s metrics:\n%s", owner, strings.Join(keep, "\n"))
	}
	t.Logf("IMPACT passed: 50ping/concurrency2,10000events/100page/2slots; idle P95=%s P99=%s loaded P95=%s P99=%s errors=0; local fixture only", idle.p95, idle.p99, loaded.p95, loaded.p99)
	client.waitOperation(ctx, backfill.CommandID)

	// Return to embedded only after all old product executors stop.
	api.stop()
	productProcess.stop()
	eventsProcess.stop()
	api = runAPI("embedded")
	panel = client.panel(ctx, eventAt-60, eventAt+60)
	if len(panel.Data.Events) != 100 || panel.Data.Summaries[0].UniqueEvents != 10000 || panel.Settings.RetentionDays != 8 {
		t.Fatalf("return preserved wrong data/settings=%+v", panel)
	}
	replay = client.accept(ctx, "/api/v1/widget/activity/settings", "settings-stable", settingsBody)
	if replay.CommandID != receipt.CommandID {
		t.Fatal("return switch lost receiver idempotency")
	}
	client.waitOperation(ctx, replay.CommandID)
	// Pilot revocation blocks new reads without restarting any component.
	_, err = owners["core"].Exec(ctx, `UPDATE activity_pilots SET enabled=false WHERE installation_id=$1`, activityTenant.InstallationID)
	mustProcess(t, err)
	status, body = client.request(ctx, "GET", fmt.Sprintf("/api/v1/widget/activity/panel?from=%d&to=%d", eventAt-60, eventAt+60), "", "")
	if status != 403 {
		t.Fatalf("revoked panel=%d %s", status, body)
	}
	status, body = second.request(ctx, "GET", "/api/v1/widget/bootstrap", "", "")
	if status != 200 {
		t.Fatalf("revocation affected other widget %d %s", status, body)
	}
	api.stop()
	fixture.stop()
	verifyProcessBudget(t, fixtureLog)
	t.Log("VERIFIED: actual API/Activity/CRM Events/Gateway processes; exclusive logical DB roles; embedded -> grpc -> embedded; retained events/settings/command IDs; Core restart and widget ping while Activity unavailable; durable202 receiver outage; old leadstatus completed using the same Gateway-owner client; pilot revocation. Synthetic amoCRM only; no production load claim.")
}

type processTenant struct {
	serviceapi.Scope
	ClientID, Secret string
}

func seedProcessTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ring *cryptox.KeyRing, pilot bool) processTenant {
	t.Helper()
	tenant := processTenant{Scope: serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}, ClientID: uuid.NewString(), Secret: "fixture-widget-secret-" + uuid.NewString()}
	sealed, v, err := ring.Seal([]byte(tenant.Secret), cryptox.IntegrationSecretAAD(tenant.IntegrationID))
	mustProcess(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,client_secret_key_version,redirect_uri)VALUES($1,$2,$2,$3,$4,'https://fixture-backend.test/oauth')`, tenant.IntegrationID, tenant.ClientID, sealed, v)
	mustProcess(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)VALUES($1,$2,42,'fixture.amocrm.ru','active')`, tenant.InstallationID, tenant.IntegrationID)
	mustProcess(t, err)
	code := "lead-status"
	if pilot {
		code = "activity"
	}
	_, err = pool.Exec(ctx, `INSERT INTO integration_services(integration_id,service_code,enabled)VALUES($1,$2,true)`, tenant.IntegrationID, code)
	mustProcess(t, err)
	if pilot {
		_, err = pool.Exec(ctx, `INSERT INTO activity_pilots(installation_id,enabled)VALUES($1,true)`, tenant.InstallationID)
		mustProcess(t, err)
	}
	return tenant
}
func processDSN(t *testing.T, base, db, role string) string {
	t.Helper()
	u, err := url.Parse(base)
	mustProcess(t, err)
	u.Path = "/" + db
	u.User = url.UserPassword(role, role+"_dev")
	return u.String()
}
func processAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	mustProcess(t, err)
	_, port, _ := net.SplitHostPort(l.Addr().String())
	mustProcess(t, l.Close())
	return "localhost:" + port
}
func mustProcess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func mergeProcessEnv(base, extra map[string]string) map[string]string {
	r := map[string]string{}
	for k, v := range base {
		r[k] = v
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

type processChild struct {
	cmd  *exec.Cmd
	done chan error
	once sync.Once
	log  string
	t    *testing.T
}

func startProcess(t *testing.T, temp, name, binary string, args []string, env map[string]string) *processChild {
	t.Helper()
	log := filepath.Join(temp, name+"-"+uuid.NewString()+".log")
	f, err := os.Create(log)
	mustProcess(t, err)
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	mustProcess(t, cmd.Start())
	p := &processChild{cmd: cmd, done: make(chan error, 1), log: log, t: t}
	go func() { p.done <- cmd.Wait(); _ = f.Close() }()
	t.Cleanup(func() {
		p.stop()
		if t.Failed() {
			body, _ := os.ReadFile(log)
			t.Logf("%s log:\n%s", name, body)
		}
	})
	return p
}
func (p *processChild) stop() {
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(12 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}
func waitProcess(t *testing.T, ctx context.Context, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if check() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("process condition did not become ready")
}
func waitHTTPProcess(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	waitProcess(t, ctx, 15*time.Second, func() bool {
		request, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
		c := http.Client{Timeout: time.Second}
		r, err := c.Do(request)
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	})
}

type processWidgetClient struct {
	base   string
	tenant processTenant
	t      *testing.T
}

func (c processWidgetClient) request(ctx context.Context, method, path, key, body string) (int, []byte) {
	c.t.Helper()
	now := time.Now()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://fixture.amocrm.ru", "aud": "https://fixture-backend.test", "iat": now.Add(-time.Second).Unix(), "nbf": now.Add(-time.Second).Unix(), "exp": now.Add(time.Minute).Unix(), "jti": uuid.NewString(), "account_id": int64(42), "user_id": int64(7), "client_uuid": c.tenant.ClientID}).SignedString([]byte(c.tenant.Secret))
	mustProcess(c.t, err)
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, strings.NewReader(body))
	mustProcess(c.t, err)
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Origin", "https://fixture.amocrm.ru")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	client := http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	mustProcess(c.t, err)
	return resp.StatusCode, data
}
func (c processWidgetClient) accept(ctx context.Context, path, key, body string) activitybridge.Receipt {
	c.t.Helper()
	status, data := c.request(ctx, "POST", path, key, body)
	if status != 202 {
		c.t.Fatalf("accept %s=%d %s", path, status, data)
	}
	var r activitybridge.Receipt
	mustProcess(c.t, json.Unmarshal(data, &r))
	return r
}
func (c processWidgetClient) waitOperation(ctx context.Context, id string) {
	c.t.Helper()
	var last []byte
	defer func() {
		if c.t.Failed() {
			c.t.Logf("operation %s last response: %s", id, last)
		}
	}()
	waitProcess(c.t, ctx, 90*time.Second, func() bool {
		time.Sleep(750 * time.Millisecond)
		status, data := c.request(ctx, "GET", "/api/v1/widget/activity/operations/"+id, "", "")
		last = data
		if status != 200 {
			return false
		}
		var r activitybridge.Receipt
		if json.Unmarshal(data, &r) != nil {
			return false
		}
		if r.State == serviceapi.OperationFailed {
			c.t.Fatalf("operation failed: %s", data)
		}
		switch r.State {
		case "pending_delivery", serviceapi.OperationAccepted, serviceapi.OperationRunning, serviceapi.OperationRetry, serviceapi.OperationPaused, serviceapi.OperationSucceeded:
		default:
			c.t.Fatalf("operation returned noncanonical public state: %s", data)
		}
		return r.State == serviceapi.OperationSucceeded
	})
	_ = last
}
func (c processWidgetClient) panel(ctx context.Context, from, to int64) serviceapi.Panel {
	c.t.Helper()
	status, data := c.request(ctx, "GET", fmt.Sprintf("/api/v1/widget/activity/panel?from=%d&to=%d", from, to), "", "")
	if status != 200 {
		c.t.Fatalf("panel=%d %s", status, data)
	}
	var panel serviceapi.Panel
	mustProcess(c.t, json.Unmarshal(data, &panel))
	return panel
}

type fixtureTokens struct{ tenants map[string]string }

func (f fixtureTokens) Token(ctx context.Context, id uuid.UUID) (amocrm.AccessToken, error) {
	integration, ok := f.tenants[id.String()]
	if !ok {
		return amocrm.AccessToken{}, fmt.Errorf("unknown fixture installation")
	}
	return amocrm.AccessToken{InstallationID: id, IntegrationID: uuid.MustParse(integration), AccountID: 42, AccountDomain: "fixture.amocrm.ru", Value: integration, TokenVersion: 1}, nil
}
func (f fixtureTokens) RefreshIfCurrent(ctx context.Context, a amocrm.AccessToken) (amocrm.AccessToken, error) {
	return a, nil
}
func (f fixtureTokens) MarkReauthRequired(context.Context, uuid.UUID, int64) error { return nil }

type fixtureHTTP struct {
	at        int64
	mu        sync.Mutex
	file      *os.File
	countFile string
	leads     map[int64]amocrm.LeadState
}

func (f *fixtureHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	_ = json.NewEncoder(f.file).Encode(map[string]any{"at": time.Now().UnixNano(), "integration": strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), "path": req.URL.Path, "method": req.Method})
	f.mu.Unlock()
	body := "{}"
	switch {
	case req.URL.Path == "/api/v4/users/7":
		body = `{"id":7,"rights":{"is_admin":true,"is_active":true}}`
	case req.URL.Path == "/api/v4/users":
		body = `{"_embedded":{"users":[{"id":7,"name":"Fixture admin","rights":{"group_id":3}}]}}`
	case req.URL.Path == "/api/v4/account":
		body = `{"id":42,"subdomain":"fixture","_embedded":{"users_groups":[{"id":3,"name":"Sales"}],"datetime_settings":{"timezone":"Europe/Moscow"}}}`
	case strings.HasPrefix(req.URL.Path, "/api/v4/leads/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(req.URL.Path, "/api/v4/leads/"), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid fixture lead")
		}
		f.mu.Lock()
		if f.leads == nil {
			f.leads = map[int64]amocrm.LeadState{}
		}
		lead, found := f.leads[id]
		if !found {
			lead = amocrm.LeadState{ID: id, PipelineID: 10, StatusID: 20}
			if id == 42 {
				lead.StatusID = 30
			}
		}
		if req.Method == http.MethodPatch {
			var change struct {
				PipelineID int64 `json:"pipeline_id"`
				StatusID   int64 `json:"status_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&change); err != nil {
				f.mu.Unlock()
				return nil, err
			}
			if change.PipelineID <= 0 || change.StatusID <= 0 {
				f.mu.Unlock()
				return nil, fmt.Errorf("invalid fixture lead mutation")
			}
			lead.PipelineID = change.PipelineID
			lead.StatusID = change.StatusID
			f.leads[id] = lead
		} else if req.Method != http.MethodGet {
			f.mu.Unlock()
			return nil, fmt.Errorf("invalid fixture lead method")
		}
		f.mu.Unlock()
		encoded, _ := json.Marshal(lead)
		body = string(encoded)
	case req.URL.Path == "/api/v4/events":
		from, _ := strconv.ParseInt(req.URL.Query().Get("filter[created_at][from]"), 10, 64)
		to, _ := strconv.ParseInt(req.URL.Query().Get("filter[created_at][to]"), 10, 64)
		body = `{"_embedded":{"events":[]}}`
		if f.at >= from && f.at <= to {
			count := 1
			if raw, err := os.ReadFile(f.countFile); err == nil {
				if value, err := strconv.Atoi(string(raw)); err == nil && value > 0 {
					count = value
				}
			}
			page, _ := strconv.Atoi(req.URL.Query().Get("page"))
			limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
			items := []serviceapi.Event{}
			for i := (page - 1) * limit; i < page*limit && i < count; i++ {
				id := "fixture-event"
				if i > 0 {
					id = fmt.Sprintf("fixture-event-%05d", i)
				}
				items = append(items, serviceapi.Event{ID: id, CreatedAt: f.at, CreatedBy: 7, Type: "lead_added", EntityID: 42, EntityType: "lead", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)})
			}
			response := map[string]any{"_embedded": map[string]any{"events": items}}
			if page*limit < count {
				response["_links"] = map[string]any{"next": map[string]string{"href": "https://fixture.amocrm.ru/api/v4/events?page=" + strconv.Itoa(page+1)}}
			}
			encoded, _ := json.Marshal(response)
			body = string(encoded)
		}
	default:
		return nil, fmt.Errorf("unapproved synthetic API path %s", req.URL.Path)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestProcessGatewayFixture(t *testing.T) {
	if os.Getenv("COMPONENT_GATEWAY_FIXTURE") != "1" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	cfg, err := Load("worker")
	mustProcess(t, err)
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	mustProcess(t, err)
	defer pool.Close()
	var tenants map[string]string
	mustProcess(t, json.Unmarshal([]byte(os.Getenv("FIXTURE_TENANTS")), &tenants))
	at, err := strconv.ParseInt(os.Getenv("FIXTURE_EVENT_AT"), 10, 64)
	mustProcess(t, err)
	log, err := os.Create(os.Getenv("FIXTURE_REQUEST_LOG"))
	mustProcess(t, err)
	defer log.Close()
	api := amocrm.NewClient(&http.Client{Transport: &fixtureHTTP{at: at, file: log, countFile: os.Getenv("FIXTURE_EVENT_COUNT_FILE")}, Timeout: 10 * time.Second}, fixtureTokens{tenants})
	reg := prometheus.NewRegistry()
	api.SetMetrics(amocrm.NewMetrics(reg))
	graph, err := StartGateway(ctx, cfg, pool, api, reg)
	mustProcess(t, err)
	defer graph.Close()
	metricsServer := &http.Server{Addr: os.Getenv("FIXTURE_METRICS_ADDRESS"), Handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), ReadHeaderTimeout: time.Second}
	go func() { _ = metricsServer.ListenAndServe() }()
	defer metricsServer.Close()
	store := jobs.NewStore(pool)
	handlers := map[string]jobs.Handler{widgetapi.PingJobType: widgetapi.PingJobHandler(widgetapi.NewExecutionStore(pool))}
	observers := map[string]jobs.FailureObserver{}
	leadstatus.NewModule(pool, store).RegisterJobs(handlers, observers, api)
	worker := jobs.NewWorker(store, slog.Default(), jobs.WorkerConfig{ID: "process-fixture", PollInterval: 50 * time.Millisecond, LeaseDuration: 20 * time.Second, JobTimeout: 10 * time.Second, BatchSize: 2, ReapBatchSize: 10, Concurrency: 2, IntegrationConcurrency: 1, DrainTimeout: 5 * time.Second, ClaimTimeout: time.Second}, handlers, observers)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-ctx.Done():
	case <-graph.Failed():
		t.Fatalf("fixture Gateway: %v", graph.Err())
	}
	cancel()
	<-done
}
func verifyProcessBudget(t *testing.T, file string) {
	t.Helper()
	data, err := os.ReadFile(file)
	mustProcess(t, err)
	byIntegration := map[string][]int64{}
	eventCalls, leadCalls, mutations := 0, 0, 0
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var r struct {
			At          int64  `json:"at"`
			Integration string `json:"integration"`
			Path        string `json:"path"`
			Method      string `json:"method"`
		}
		mustProcess(t, json.Unmarshal(line, &r))
		byIntegration[r.Integration] = append(byIntegration[r.Integration], r.At)
		if r.Path == "/api/v4/events" {
			eventCalls++
		}
		if strings.HasPrefix(r.Path, "/api/v4/leads/") {
			leadCalls++
			if r.Method == http.MethodPatch {
				mutations++
			}
		}
	}
	if mutations != 60 {
		t.Fatalf("expected60actual lead PATCH effects, got%d", mutations)
	}
	if eventCalls == 0 || leadCalls == 0 {
		t.Fatalf("common owner did not execute both products: events=%d leads=%d", eventCalls, leadCalls)
	}
	for _, timestamps := range byIntegration {
		sort.Slice(timestamps, func(i, j int) bool { return timestamps[i] < timestamps[j] })
		for i, start := range timestamps {
			for j := i; j < len(timestamps); j++ {
				elapsed := float64(timestamps[j]-start) / float64(time.Second)
				if float64(j-i+1) > 7+7*elapsed+1.1 {
					t.Fatalf("shared integration limiter burst/rate violated: %d requests in %.3fs", j-i+1, elapsed)
				}
			}
		}
	}
	t.Logf("synthetic common-owner outgoing samples: Events=%d LeadRequests=%d LeadPATCH=%d integrations=%d; all intervals satisfy 7 burst + 7rps (+1 scheduling tolerance)", eventCalls, leadCalls, mutations, len(byIntegration))
}

type processLatency struct{ p95, p99 time.Duration }

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
func processPingWorkload(t *testing.T, ctx context.Context, client processWidgetClient, pool *pgxpool.Pool, prefix string) processLatency {
	t.Helper()
	type sample struct {
		latency time.Duration
		status  int
		id      string
	}
	out := make(chan sample, 50)
	var wg sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := worker; i < 50; i += 2 {
				start := time.Now()
				status, body := client.request(ctx, "POST", "/api/v1/widget/actions/ping", fmt.Sprintf("impact-%s-%d", prefix, i), "")
				var receipt struct {
					JobID string `json:"job_id"`
				}
				_ = json.Unmarshal(body, &receipt)
				out <- sample{time.Since(start), status, receipt.JobID}
			}
		}(worker)
	}
	wg.Wait()
	close(out)
	latencies := []time.Duration{}
	ids := []string{}
	for item := range out {
		if item.status != 202 || item.id == "" {
			t.Fatalf("impact ping returned%d", item.status)
		}
		latencies = append(latencies, item.latency)
		ids = append(ids, item.id)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	waitProcess(t, ctx, 20*time.Second, func() bool {
		var count int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id::text=ANY($1) AND status='completed'`, ids).Scan(&count)
		return err == nil && count == 50
	})
	return processLatency{latencies[47], latencies[49]}
}
func processMetrics(t *testing.T, ctx context.Context, address string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	mustProcess(t, err)
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	mustProcess(t, err)
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("metrics status%d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	mustProcess(t, err)
	return string(body)
}

// Product completion uses committed jobs.created_at/finished_at, not acceptance
// alone. A new request starts only after its caller observes durable completion.
type productLatency struct{ admission, completion processLatency }

func processProductWorkload(t *testing.T, ctx context.Context, client processWidgetClient, core, events *pgxpool.Pool, phase string, firstLead int64) productLatency {
	t.Helper()
	type sample struct {
		admission, completion time.Duration
		err                   error
	}
	out := make(chan sample, 30)
	var wg sync.WaitGroup
	activeBackfill := func() error {
		if events == nil {
			return nil
		}
		var active int
		err := events.QueryRow(ctx, `SELECT count(*) FROM event_jobs WHERE kind='backfill' AND status IN('running','queued')`).Scan(&active)
		if err != nil {
			return err
		}
		if active == 0 {
			return fmt.Errorf("10000-event backfill stopped during loaded product measurement")
		}
		return nil
	}
	for caller := 0; caller < 2; caller++ {
		wg.Add(1)
		go func(caller int) {
			defer wg.Done()
			for i := caller; i < 30; i += 2 {
				if err := activeBackfill(); err != nil {
					out <- sample{err: err}
					return
				}
				leadID := firstLead + int64(i)
				targetStatus := int64(100 + i)
				started := time.Now()
				status, body := client.request(ctx, "POST", "/api/v1/widget/actions/leads/set-status", fmt.Sprintf("product-impact-%s-%d", phase, i), fmt.Sprintf(`{"lead_id":%d,"pipeline_id":10,"status_id":%d}`, leadID, targetStatus))
				admission := time.Since(started)
				var receipt struct {
					JobID string `json:"job_id"`
				}
				if status != 202 {
					out <- sample{err: fmt.Errorf("leadstatus admission=%d %s", status, body)}
					return
				}
				if err := json.Unmarshal(body, &receipt); err != nil || receipt.JobID == "" {
					out <- sample{err: fmt.Errorf("leadstatus receipt invalid")}
					return
				}
				deadline := time.Now().Add(6 * time.Second)
				finished := false
				for time.Now().Before(deadline) && ctx.Err() == nil {
					var state string
					var created time.Time
					var ended *time.Time
					var attempts int
					var payload []byte
					err := core.QueryRow(ctx, `SELECT status,created_at,finished_at,attempts,result FROM jobs WHERE id=$1`, receipt.JobID).Scan(&state, &created, &ended, &attempts, &payload)
					if err != nil {
						out <- sample{err: err}
						return
					}
					if state == "completed" && ended != nil {
						var result leadstatus.LeadStatusResult
						if err := json.Unmarshal(payload, &result); err != nil || !result.Converged || result.LeadID != leadID || result.StatusID != targetStatus || attempts != 1 {
							out <- sample{err: fmt.Errorf("incorrect completed leadstatus result/attempts: %s attempts=%d", payload, attempts)}
							return
						}
						if err := activeBackfill(); err != nil {
							out <- sample{err: err}
							return
						}
						out <- sample{admission: admission, completion: ended.Sub(created)}
						finished = true
						break
					}
					if state == "failed" || state == "dead" || state == "cancelled" || state == "retry" || attempts > 1 {
						out <- sample{err: fmt.Errorf("leadstatus lifecycle error state=%s attempts=%d", state, attempts)}
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
				if !finished {
					out <- sample{err: fmt.Errorf("leadstatus completion exceeded6s")}
					return
				}
			}
		}(caller)
	}
	wg.Wait()
	close(out)
	admissions, completions := []time.Duration{}, []time.Duration{}
	for item := range out {
		if item.err != nil {
			t.Fatal(item.err)
		}
		admissions = append(admissions, item.admission)
		completions = append(completions, item.completion)
	}
	if len(admissions) != 30 {
		t.Fatalf("product phase completed%d/30 jobs", len(admissions))
	}
	sort.Slice(admissions, func(i, j int) bool { return admissions[i] < admissions[j] })
	sort.Slice(completions, func(i, j int) bool { return completions[i] < completions[j] })
	result := productLatency{admission: processLatency{admissions[28], admissions[29]}, completion: processLatency{completions[28], completions[29]}}
	if result.admission.p95 >= time.Second || result.admission.p99 >= 2*time.Second || result.completion.p95 >= 3*time.Second || result.completion.p99 >= 5*time.Second {
		t.Fatalf("prespecified %s product latency thresholds exceeded: %+v", phase, result)
	}
	var backlog int
	var oldest float64
	mustProcess(t, core.QueryRow(ctx, `SELECT count(*),coalesce(extract(epoch FROM now()-min(created_at)),0)::float8 FROM jobs WHERE status IN('queued','processing','retry')`).Scan(&backlog, &oldest))
	t.Logf("product %s Corejob snapshot: backlog=%d oldest_age_seconds=%.3f;30completed,0errors,0retries", phase, backlog, oldest)
	return result
}
