package productplatform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/admincommand"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const pilotAccount int64 = 9007199254740993
const legacyAccount int64 = pilotAccount + 1
const continuationAccount int64 = pilotAccount + 2

func TestPlatformLifecycleAcrossOwners(t *testing.T) {
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")
	ownerDSN := os.Getenv("PLATFORM_TEST_OWNER_DSN")
	writerDSN := os.Getenv("PLATFORM_TEST_JOBS_DSN")
	if certs == "" || ownerDSN == "" || writerDSN == "" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("requires separate migrated Core and Platform test databases and generated Platform mTLS certificates")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	core := testkit.Postgres(t)
	testkit.Reset(t, core)
	owner, err := storage.Open(ctx, ownerDSN, storage.Owner, 2)
	require(t, err)
	defer owner.Close()
	writer, err := storage.Open(ctx, writerDSN, storage.Jobs, 3)
	require(t, err)
	defer writer.Close()

	_, err = owner.Exec(ctx, `TRUNCATE platform_lifecycle.manifest, platform_lifecycle.products,
		platform_lifecycle.accounts, platform_lifecycle.cursors, platform_lifecycle.inbox,
		platform_lifecycle.reconciliation, platform_jobs.jobs CASCADE`)
	require(t, err)

	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{7}, 32)}, 1)
	require(t, err)
	oauthStore := oauth.NewStore(core, keys)
	integration := createIntegration(t, ctx, oauthStore, "pilot-one")
	collision := createIntegration(t, ctx, oauthStore, "pilot-two")
	legacy := uuid.New()
	_, err = core.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status)
		VALUES($1,$2,$3,'legacy.amocrm.ru','active')`, legacy, integration.ID, legacyAccount)
	require(t, err)

	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: []wire.Mapping{
		{Integration: integration.ID, ProductCode: "pilot", Accounts: []string{account(pilotAccount), account(legacyAccount), account(continuationAccount)}},
		{Integration: collision.ID, ProductCode: "pilot", Accounts: []string{account(pilotAccount)}},
	}})
	require(t, err)
	journal := corelifecycle.NewStore(core)
	require(t, productplatform.BootstrapLifecycle(ctx, owner, manifest))
	require(t, journal.Bootstrap(ctx, manifest))
	require(t, journal.Bootstrap(ctx, manifest))
	require(t, productplatform.BootstrapLifecycle(ctx, owner, manifest))

	_, err = core.Exec(ctx, `CREATE FUNCTION p03_reject_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.type='platform.lifecycle_delivery' THEN RAISE EXCEPTION 'test enqueue unavailable'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER p03_reject_delivery BEFORE INSERT ON jobs FOR EACH ROW EXECUTE FUNCTION p03_reject_delivery();`)
	require(t, err)
	t.Cleanup(func() {
		_, _ = core.Exec(context.Background(), `DROP FUNCTION IF EXISTS p03_reject_delivery() CASCADE`)
	})
	if _, err = oauthStore.SaveInstallation(ctx, integration, oauth.Account{ID: pilotAccount}, "pilot.amocrm.ru", credential()); err == nil {
		t.Fatal("OAuth committed despite failed Platform enqueue")
	}
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM installations WHERE account_id=$1`, pilotAccount)
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM oauth_credentials`)
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM platform_lifecycle_ledger`)
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM jobs`)
	_, err = core.Exec(ctx, `DROP FUNCTION p03_reject_delivery() CASCADE`)
	require(t, err)

	snapshot, err := corelifecycle.NewSnapshotService(journal, filepath.Join(certs, "core/snapshot-delegation.key"))
	require(t, err)
	snapshotRPC := startSnapshot(t, snapshot, certs)
	platformRPC := snapshotRPC("platform")
	legacyToken, err := snapshot.Issue(ctx, legacy, integration.ID)
	require(t, err)
	legacyView, err := platformRPC.GetSnapshot(ctx, snapshotRequest(legacy, legacyToken))
	require(t, err)
	if legacyView.Authorization != platformpb.Authorization_AUTHORIZATION_UNKNOWN || legacyView.Generation != nil || legacyView.Sequence != nil {
		t.Fatalf("legacy snapshot fabricated credentials or lifecycle: %v", legacyView)
	}
	assertCount(t, ctx, core, 1, `SELECT count(*) FROM platform_lifecycle_reconciliation WHERE installation_id=$1 AND reason='history_unproven'`, legacy)
	save := func(i oauth.Integration, id int64) uuid.UUID {
		t.Helper()
		result, err := oauthStore.SaveInstallation(ctx, i, oauth.Account{ID: id}, "pilot.amocrm.ru", credential())
		require(t, err)
		return result.ID
	}
	if id := save(integration, legacyAccount); id != legacy {
		t.Fatal("legacy authorization changed installation identity")
	}
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM platform_lifecycle_ledger WHERE installation_id=$1`, legacy)
	unscoped := save(integration, legacyAccount+10)
	if _, err := snapshot.Issue(ctx, unscoped, integration.ID); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("unscoped snapshot issued: %v", err)
	}
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM platform_lifecycle_ledger WHERE installation_id=$1`, unscoped)
	installation := save(integration, pilotAccount)
	if id := save(integration, pilotAccount); id != installation {
		t.Fatal("reauthorization replaced Core identity")
	}

	auth, err := snapshot.Issue(ctx, installation, integration.ID)
	require(t, err)
	s, err := platformRPC.GetSnapshot(ctx, snapshotRequest(installation, auth))
	require(t, err)
	if s.GetGeneration() != 1 || s.GetSequence() != 2 || s.GetAuthorizationSequence() != 2 || s.GetAmoAccountId() != pilotAccount ||
		s.GetCurrentDomain() != "pilot.amocrm.ru" || s.Authorization != platformpb.Authorization_AUTHORIZATION_AUTHORIZED {
		t.Fatalf("incorrect scoped snapshot: %v", s)
	}
	if _, err := platformRPC.GetSnapshot(ctx, snapshotRequest(legacy, auth)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("snapshot delegation could be mistargeted: %v", err)
	}
	if _, err := snapshotRPC("core").GetSnapshot(ctx, snapshotRequest(installation, auth)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Core certificate accepted as Platform: %v", err)
	}
	if _, err := platformRPC.GetSnapshot(ctx, snapshotRequest(installation, "")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("snapshot accepted absent delegation: %v", err)
	}

	inbox := productplatform.NewLifecycleStore(writer)
	server := startInbox(t, inbox, certs)
	coreHTTP := httpClient(t, certs, "core")
	managerHTTP := httpClient(t, certs, "manager-bff")
	handler, err := corelifecycle.DeliveryHandler(journal, server, filepath.Join(certs, "core"))
	require(t, err)
	deliver := func(id uuid.UUID, sequence int64) []byte {
		t.Helper()
		var jid uuid.UUID
		require(t, core.QueryRow(ctx, `SELECT j.id FROM jobs j JOIN platform_lifecycle_ledger e ON j.resource_id=e.event_id::text
			WHERE e.installation_id=$1 AND e.sequence=$2 AND j.type=$3 ORDER BY j.created_at DESC LIMIT 1`,
			id, sequence, corelifecycle.JobType).Scan(&jid))
		job, err := jobs.NewStore(core).GetForInstallation(ctx, jid, id)
		require(t, err)
		receipt, err := handler(ctx, job)
		require(t, err)
		return receipt
	}
	body := func(id uuid.UUID, seq int64) []byte {
		t.Helper()
		var raw []byte
		require(t, core.QueryRow(ctx, `SELECT body FROM platform_lifecycle_ledger WHERE installation_id=$1 AND sequence=$2`, id, seq).Scan(&raw))
		return raw
	}
	first, second := body(installation, 1), body(installation, 2)
	post(t, managerHTTP, server, first, http.StatusForbidden)
	recorded := deliver(installation, 2)
	assertCount(t, ctx, owner, 0, `SELECT count(*) FROM platform_lifecycle.episodes`)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.reconciliation WHERE reason='sequence_gap' AND resolved_at IS NULL`)
	deliver(installation, 1)
	assertCount(t, ctx, owner, 0, `SELECT count(*) FROM platform_lifecycle.reconciliation WHERE reason='sequence_gap' AND resolved_at IS NULL`)
	assertCount(t, ctx, owner, 2, `SELECT count(*) FROM platform_lifecycle.history`)
	if got := post(t, coreHTTP, server, second, http.StatusAccepted); !bytes.Equal(bytes.TrimSpace(got), recorded) {
		t.Fatalf("duplicate changed receipt: %s %s", got, recorded)
	}
	conflict, err := wire.DecodeEvent(second)
	require(t, err)
	conflict.OccurredAt = conflict.OccurredAt.Add(time.Second)
	post(t, coreHTTP, server, eventBody(t, conflict), http.StatusConflict)
	conflict.ID = uuid.New()
	post(t, coreHTTP, server, eventBody(t, conflict), http.StatusConflict)
	conflict.Installation, conflict.Account = unscoped, account(legacyAccount+10)
	post(t, coreHTTP, server, eventBody(t, conflict), http.StatusNotFound)
	assertCount(t, ctx, owner, 0, `SELECT count(*) FROM platform_lifecycle.inbox WHERE installation_id=$1`, unscoped)
	collidingInstallation := save(collision, pilotAccount)
	deliver(collidingInstallation, 1)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.reconciliation WHERE installation_id=$1 AND reason='account_product_conflict'`, collidingInstallation)

	termUntil := time.Now().UTC().Truncate(time.Second).Add(30 * 24 * time.Hour)
	_, err = owner.Exec(ctx, `INSERT INTO platform_lifecycle.terms(account_id,product_id,valid_until,source)
		SELECT account_id,product_id,$2,'test' FROM platform_lifecycle.account_products WHERE installation_id=$1`, installation, termUntil)
	require(t, err)
	var firstEpisode uuid.UUID
	require(t, owner.QueryRow(ctx, `SELECT id FROM platform_lifecycle.episodes WHERE installation_id=$1 AND generation=1`, installation).Scan(&firstEpisode))

	operator := integrations.NewStore(core, keys)
	command := func(action string) integrations.Result {
		t.Helper()
		c := integrations.Command{Action: action, Actor: "pilot-test", Code: integration.Code}
		if action != "disable" && action != "enable" {
			c.InstallationID = installation
		}
		result, err := operator.Apply(ctx, c)
		require(t, err)
		return result
	}
	command("disable")
	disabled, err := platformRPC.GetSnapshot(ctx, snapshotRequest(installation, auth))
	require(t, err)
	if disabled.Authorization != platformpb.Authorization_AUTHORIZATION_DISABLED || disabled.GetSequence() != 3 {
		t.Fatalf("live snapshot ignored disable: %v", disabled)
	}
	command("enable")
	command("revoke")
	save(integration, pilotAccount)
	provider := oauth.NewTokenProvider(core, keys, nil)
	require(t, provider.MarkReauthRequired(ctx, installation, 2))
	assertCount(t, ctx, core, 6, `SELECT sequence FROM platform_lifecycle_cursors WHERE installation_id=$1`, installation)
	var version int64
	require(t, core.QueryRow(ctx, `SELECT token_version FROM oauth_credentials WHERE installation_id=$1`, installation).Scan(&version))
	require(t, provider.MarkReauthRequired(ctx, installation, version))
	assertCount(t, ctx, core, 7, `SELECT sequence FROM platform_lifecycle_cursors WHERE installation_id=$1`, installation)
	for i := int64(3); i <= 7; i++ {
		deliver(installation, i)
	}

	reconcile := webhook.NewReconcileStore(core, keys)
	oldAttempt := command("uninstall").UninstallAttempt
	if oldAttempt == uuid.Nil || command("uninstall").UninstallAttempt != oldAttempt {
		t.Fatal("uninstall replay lost pending attempt identity")
	}
	admin := admincommand.NewStore(core, keys, 5*time.Second, "", nil, nil)
	accepted, err := admin.Execute(ctx, "pilot-test", uuid.NewString(), admincommand.Request{
		TargetType: "installation", TargetID: installation.String(), Command: "uninstall", Payload: json.RawMessage(`{}`),
	})
	require(t, err)
	if accepted.State != "pending" || accepted.JobID == nil {
		t.Fatalf("Admin uninstall was not durably accepted: %+v", accepted)
	}
	acceptedJob, err := jobs.NewStore(core).GetForInstallation(ctx, *accepted.JobID, installation)
	require(t, err)
	deliver(installation, 8)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.episodes WHERE id=$1 AND closed_at IS NULL`, firstEpisode)
	matching := destination(t, ctx, core, keys, installation)
	gateway := &cleanupGateway{remote: []amocrm.Webhook{{Destination: matching}}, deleteError: errors.New("remote outcome unknown")}
	if err := webhook.UnregisterAttempt(ctx, reconcile, gateway, installation, oldAttempt); err == nil {
		t.Fatal("partial uninstall reported confirmed")
	}
	assertCount(t, ctx, core, 0, `SELECT count(*) FROM platform_lifecycle_ledger WHERE evidence_id=$1`, oldAttempt)
	assertCount(t, ctx, core, 1, `SELECT count(*) FROM platform_lifecycle_reconciliation WHERE installation_id=$1 AND reason='uninstall_pending' AND resolved_at IS NULL`, installation)

	gateway.deleteError = nil
	gateway.afterDelete = func() { save(integration, pilotAccount) }
	if err := webhook.UnregisterAttempt(ctx, reconcile, gateway, installation, oldAttempt); !errors.Is(err, corelifecycle.ErrStaleUninstall) {
		t.Fatalf("stale attempt finished after OAuth: %v", err)
	}
	if destination(t, ctx, core, keys, installation) == matching {
		t.Fatal("replacement OAuth reused the old webhook destination key")
	}
	deliver(installation, 9)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.episodes WHERE id=$1 AND closed_at IS NULL`, firstEpisode)

	var secondAttemptID uuid.UUID
	newDestination := destination(t, ctx, core, keys, installation)
	gateway = &cleanupGateway{remote: []amocrm.Webhook{{Destination: newDestination}, {Destination: "https://hooks.example.test/hooks/amocrm/v1/foreign"}}}
	secondAttemptID = command("uninstall").UninstallAttempt
	if secondAttemptID == oldAttempt {
		t.Fatal("new uninstall reused an attempt from the prior authorization")
	}
	executor := admincommand.NewWorkerExecutor(core, nil, keys, nil)
	executor.Unregister = func(ctx context.Context, id, attempt uuid.UUID) error {
		if id != installation || attempt != oldAttempt {
			t.Fatalf("Admin resolved a different uninstall than the one accepted: %s, %s", id, attempt)
		}
		err := webhook.UnregisterAttempt(ctx, reconcile, gateway, id, attempt)
		if !errors.Is(err, corelifecycle.ErrStaleUninstall) {
			t.Fatalf("Admin accepted a stale uninstall: %v", err)
		}
		return err
	}
	result, err := executor.Handler(ctx, acceptedJob)
	require(t, err)
	if !bytes.Contains(result, []byte(`"state":"partial"`)) || gateway.lists != 0 {
		t.Fatalf("old accepted Admin job touched amoCRM: calls=%d receipt=%s", gateway.lists, result)
	}
	_, err = core.Exec(ctx, `UPDATE jobs SET status='processing',attempts=1,locked_by='test-reconcile',locked_until=now()+interval '10 seconds'
		WHERE id=(SELECT id FROM jobs WHERE installation_id=$1 AND type='webhook.reconcile' ORDER BY created_at DESC LIMIT 1)`, installation)
	require(t, err)
	if err := webhook.UnregisterAttempt(ctx, reconcile, gateway, installation, secondAttemptID); err == nil || gateway.lists != 0 {
		t.Fatalf("uninstall raced known conflicting registration: calls=%d err=%v", gateway.lists, err)
	}
	_, err = core.Exec(ctx, `UPDATE jobs SET status='queued',locked_by=NULL,locked_until=NULL
		WHERE installation_id=$1 AND locked_by='test-reconcile'`, installation)
	require(t, err)
	require(t, webhook.UnregisterAttempt(ctx, reconcile, gateway, installation, secondAttemptID))
	if len(gateway.remote) != 1 || len(gateway.deleted) != 1 || gateway.deleted[0] != newDestination {
		t.Fatalf("uninstall cleanup exceeded owned destination: %#v", gateway)
	}
	seq := int64(11)
	assertCount(t, ctx, core, 1, `SELECT count(*) FROM platform_lifecycle_ledger WHERE evidence_id=$1 AND sequence=$2`, secondAttemptID, seq)
	deliver(installation, 10)
	deliver(installation, 11)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.episodes WHERE id=$1 AND closed_at IS NOT NULL`, firstEpisode)
	require(t, webhook.UnregisterAttempt(ctx, reconcile, gateway, installation, secondAttemptID))

	save(integration, pilotAccount)
	deliver(installation, 12)
	post(t, coreHTTP, server, body(installation, 11), http.StatusAccepted)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.episodes WHERE installation_id=$1 AND generation=2 AND id<>$2 AND closed_at IS NULL`, installation, firstEpisode)
	assertCount(t, ctx, owner, 1, `SELECT count(*) FROM platform_lifecycle.terms t JOIN platform_lifecycle.account_products p USING(account_id,product_id)
		WHERE p.installation_id=$1 AND t.valid_until=$2 AND t.source='test'`, installation, termUntil)
	assertCount(t, ctx, owner, 12, `SELECT sequence FROM platform_lifecycle.cursors WHERE installation_id=$1`, installation)

	_, err = core.Exec(ctx, `DELETE FROM jobs WHERE type=$1 AND installation_id=$2`, corelifecycle.JobType, installation)
	require(t, err)
	require(t, journal.Replay(ctx, installation, 8, 12))
	deliver(installation, 11)
	assertCount(t, ctx, owner, 12, `SELECT count(*) FROM platform_lifecycle.history h JOIN platform_lifecycle.inbox i USING(event_id) WHERE i.installation_id=$1`, installation)
	if err := journal.Replay(ctx, installation, 12, 13); err == nil {
		t.Fatal("replay generated a missing original")
	}

	checkBoundedDrain(t, ctx, owner, writer, inbox, coreHTTP, server, integration.ID)
}

func checkBoundedDrain(t *testing.T, ctx context.Context, owner, writer *pgxpool.Pool, inbox *productplatform.LifecycleStore,
	client *http.Client, server string, integration uuid.UUID) {
	t.Helper()
	installation := uuid.New()
	e := wire.Event{Version: 1, Account: account(continuationAccount), Integration: integration, Installation: installation,
		Kind: wire.Reauthorized, OccurredAt: time.Now().UTC(), Generation: 1, Evidence: wire.Evidence{Source: "core_oauth"}}
	const last = 26
	for sequence := int64(2); sequence <= last; sequence++ {
		e.Sequence, e.ID, e.Evidence.ID = sequence, uuid.New(), uuid.New()
		post(t, client, server, eventBody(t, e), http.StatusAccepted)
	}
	e.Sequence, e.ID, e.Evidence.ID, e.Kind = 1, uuid.New(), uuid.New(), wire.Installed
	post(t, client, server, eventBody(t, e), http.StatusAccepted)
	assertCount(t, ctx, owner, 24, `SELECT sequence FROM platform_lifecycle.cursors WHERE installation_id=$1`, installation)
	queueStore := queue.NewStore(writer)
	work, found, err := queueStore.Claim(ctx, []queue.Kind{productplatform.DrainKind}, time.Second)
	require(t, err)
	if !found {
		t.Fatal("missing durable Platform continuation")
	}
	require(t, inbox.DrainJob(ctx, work.Job))
	_, err = queueStore.Finish(ctx, work, nil)
	require(t, err)
	assertCount(t, ctx, owner, last, `SELECT sequence FROM platform_lifecycle.cursors WHERE installation_id=$1`, installation)
	assertCount(t, ctx, owner, 0, `SELECT count(*) FROM platform_lifecycle.reconciliation WHERE installation_id=$1 AND resolved_at IS NULL`, installation)
}

func startInbox(t *testing.T, inbox *productplatform.LifecycleStore, certs string) string {
	t.Helper()
	dir := filepath.Join(certs, "platform")
	cfg, err := productplatform.PrivateTLS(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt"))
	require(t, err)
	server := httptest.NewUnstartedServer(productplatform.PrivateHandler(productplatform.LifecycleHandler(inbox), nil))
	server.TLS = cfg
	server.StartTLS()
	t.Cleanup(server.Close)
	return fmt.Sprintf("https://localhost:%d", server.Listener.Addr().(*net.TCPAddr).Port)
}

func startSnapshot(t *testing.T, api *corelifecycle.SnapshotService, certs string) func(string) platformpb.CoreInstallationsClient {
	t.Helper()
	dir := filepath.Join(certs, "core")
	cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "", true)
	require(t, err)
	server := servicerpc.NewServer(cfg, &servicerpc.Endpoints{PlatformInstallations: api})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return func(identity string) platformpb.CoreInstallationsClient {
		t.Helper()
		dir := filepath.Join(certs, identity)
		cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "core", false)
		require(t, err)
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		require(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return platformpb.NewCoreInstallationsClient(conn)
	}
}

func httpClient(t *testing.T, certs, identity string) *http.Client {
	t.Helper()
	dir := filepath.Join(certs, identity)
	cfg, err := servicerpc.TLSFromFiles(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), "platform", false)
	require(t, err)
	transport := &http.Transport{TLSClientConfig: cfg}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Timeout: 5 * time.Second, Transport: transport}
}

func post(t *testing.T, client *http.Client, server string, body []byte, want int) []byte {
	t.Helper()
	response, err := client.Post(server+wire.EventsPath, "application/json", bytes.NewReader(body))
	require(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require(t, err)
	if response.StatusCode != want {
		t.Fatalf("inbox responded %d, want %d: %s", response.StatusCode, want, raw)
	}
	return raw
}

func snapshotRequest(installation uuid.UUID, auth string) *platformpb.GetSnapshotRequest {
	return &platformpb.GetSnapshotRequest{CoreInstallationId: installation.String(), Auth: auth}
}

func createIntegration(t *testing.T, ctx context.Context, store *oauth.Store, code string) oauth.Integration {
	t.Helper()
	v, err := store.EnsureIntegration(ctx, oauth.IntegrationInput{Code: code, ClientID: uuid.NewString(), ClientSecret: "local-test-secret",
		RedirectURI: "https://core.test/oauth/amocrm/callback", WebhookEvents: []string{"add_lead"}})
	require(t, err)
	return v
}

func credential() oauth.Token {
	return oauth.Token{AccessToken: "local-test-access", RefreshToken: "local-test-refresh", ExpiresIn: 7200}
}

func account(id int64) string { return strconv.FormatInt(id, 10) }

func eventBody(t *testing.T, e wire.Event) []byte {
	t.Helper()
	raw, err := json.Marshal(e)
	require(t, err)
	return raw
}

func destination(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keys *cryptox.KeyRing, id uuid.UUID) string {
	t.Helper()
	var sealed []byte
	var version int
	require(t, pool.QueryRow(ctx, `SELECT webhook_key_ciphertext,webhook_key_key_version FROM installations WHERE id=$1`, id).Scan(&sealed, &version))
	plain, err := keys.Open(version, sealed, cryptox.InstallationWebhookKeyAAD(id))
	require(t, err)
	defer clear(plain)
	return "https://hooks.example.test/hooks/amocrm/v1/" + string(plain)
}

type cleanupGateway struct {
	remote      []amocrm.Webhook
	deleted     []string
	lists       int
	deleteError error
	afterDelete func()
}

func (g *cleanupGateway) ListWebhooksForUninstall(context.Context, uuid.UUID) ([]amocrm.Webhook, error) {
	g.lists++
	return g.remote, nil
}

func (g *cleanupGateway) ListWebhooks(ctx context.Context, id uuid.UUID, _ string) ([]amocrm.Webhook, error) {
	return g.ListWebhooksForUninstall(ctx, id)
}

func (*cleanupGateway) RegisterWebhook(context.Context, uuid.UUID, amocrm.WebhookSpec) (amocrm.Webhook, error) {
	return amocrm.Webhook{}, errors.New("unexpected webhook registration")
}

func (g *cleanupGateway) DeleteWebhook(_ context.Context, _ uuid.UUID, destination string) error {
	g.deleted = append(g.deleted, destination)
	if g.deleteError != nil {
		return g.deleteError
	}
	for i, v := range g.remote {
		if v.Destination == destination {
			g.remote = append(g.remote[:i], g.remote[i+1:]...)
			break
		}
	}
	if g.afterDelete != nil {
		g.afterDelete()
	}
	return nil
}

func assertCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int, query string, args ...any) {
	t.Helper()
	var got int
	require(t, pool.QueryRow(ctx, query, args...).Scan(&got))
	if got != want {
		t.Fatalf("%s returned %d, want %d", query, got, want)
	}
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
