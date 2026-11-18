package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/platformaccess"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/storage"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"github.com/sk1fy/amocrm-pro/internal/widgetcors"
	"github.com/sk1fy/amocrm-pro/internal/widgetlimit"
)

const proofTestOrigin = "https://proof.amocrm.ru"
const proofTestAccount = int64(9007199254740993)

func TestPlatformAccessAcrossOwners(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_BINARY") == "" || os.Getenv("TEST_DATABASE_URL") == "" ||
		os.Getenv("PLATFORM_TEST_CERTS_DIR") == "" || os.Getenv("PLATFORM_TEST_OWNER_DSN") == "" ||
		os.Getenv("PLATFORM_TEST_ACCESS_DSN") == "" || os.Getenv("PLATFORM_TEST_JOBS_DSN") == "" {
		t.Skip("run make platform-lifecycle-test for the two-database Platform process check")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	core := testkit.Postgres(t)
	testkit.Reset(t, core)
	owner, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"), storage.Owner, 2)
	checkAccessTest(t, err)
	defer owner.Close()
	_, err = owner.Exec(ctx, `TRUNCATE platform_lifecycle.manifest, platform_lifecycle.products,
		platform_lifecycle.accounts, platform_lifecycle.cursors, platform_lifecycle.inbox,
		platform_lifecycle.reconciliation, platform_jobs.jobs CASCADE`)
	checkAccessTest(t, err)
	keys, err := cryptox.NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{4}, 32)}, 1)
	checkAccessTest(t, err)
	oauthStore := oauth.NewStore(core, keys)
	pilot := newAccessPilot(t, ctx, oauthStore, "pilot", proofTestAccount)
	withoutTerm := newAccessPilot(t, ctx, oauthStore, "no-policy", proofTestAccount+1)
	pending := newAccessPilot(t, ctx, oauthStore, "pending", proofTestAccount+2)
	historical := newAccessPilot(t, ctx, oauthStore, "historical", proofTestAccount+3)
	free := newAccessPilot(t, ctx, oauthStore, "free-pilot", proofTestAccount+5)
	pro := newAccessPilot(t, ctx, oauthStore, "pro-pilot", proofTestAccount+6)
	ambiguous := newAccessPilot(t, ctx, oauthStore, "ambiguous-pilot", proofTestAccount+7)
	collision := newAccessPilot(t, ctx, oauthStore, "collision-pilot", ambiguous.account)
	collision.product = ambiguous.product
	unmapped := newAccessPilot(t, ctx, oauthStore, "unmapped", proofTestAccount+4)
	historical.save(t, ctx, oauthStore)
	var mappings []wire.Mapping
	for _, fixture := range []accessPilot{pilot, withoutTerm, pending, historical, free, pro, ambiguous, collision} {
		mappings = append(mappings, wire.Mapping{
			Integration: fixture.integration.ID, ProductCode: fixture.product, Accounts: []string{fixture.selector()},
		})
	}
	manifest, err := json.Marshal(wire.Manifest{Version: 1, Integrations: mappings})
	checkAccessTest(t, err)
	journal := corelifecycle.NewStore(core)
	checkAccessTest(t, journal.Bootstrap(ctx, manifest))
	checkAccessTest(t, productplatform.BootstrapLifecycle(ctx, owner, manifest))
	_, err = owner.Exec(ctx, `
		INSERT INTO platform_lifecycle.initial_term_policies(product_id,rule,version,evidence)
		SELECT id,'top_90d',4,'catalog/pilot/version-4' FROM platform_lifecycle.products WHERE code='pilot'
		UNION ALL SELECT id,'free',1,'catalog/free/version-1' FROM platform_lifecycle.products WHERE code='free-pilot'
		UNION ALL SELECT id,'pro_14d',1,'confirmed/pro/version-1' FROM platform_lifecycle.products WHERE code='pro-pilot'
		UNION ALL SELECT id,'top_90d',1,'catalog/historical/version-1' FROM platform_lifecycle.products WHERE code='historical'`)
	checkAccessTest(t, err)
	for _, fixture := range []*accessPilot{&pilot, &withoutTerm, &pending, &historical, &free, &pro, &ambiguous, &unmapped} {
		fixture.save(t, ctx, oauthStore)
	}

	oldSigner, oldPub := makeAccessKey(t, "old")
	newSigner, newPub := makeAccessKey(t, "new")
	public, private, management, stop := startAccessPlatform(t, ctx, map[string]string{"old": oldPub, "new": newPub})
	defer stop()
	client := &http.Client{Timeout: 3 * time.Second}
	requestAccess(t, client, http.MethodGet, public+"/ready", "", nil, http.StatusOK, "")
	metrics := requestAccess(t, client, http.MethodGet, management+"/metrics", "", nil, http.StatusOK, "")
	if bytes.Contains(metrics, []byte("platform_queue_")) {
		t.Fatal("worker or queue maintenance started with PLATFORM_WORKERS=0")
	}
	delivery, err := corelifecycle.DeliveryHandler(journal, private, filepath.Join(os.Getenv("PLATFORM_TEST_CERTS_DIR"), "core"))
	checkAccessTest(t, err)
	deliver := func(p accessPilot, sequence int64) {
		t.Helper()
		deliverAccessEvent(t, ctx, core, delivery, p.installation, sequence)
	}
	for _, p := range []accessPilot{pilot, withoutTerm, free, pro} {
		deliver(p, 1)
	}
	deliver(pilot, 1)

	coreA := startProofCore(t, core, keys, oldSigner, 100)
	coreB := startProofCore(t, core, keys, oldSigner, 100)
	preflight := map[string]string{"Origin": proofTestOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "X-Auth-Token"}
	requestAccess(t, client, http.MethodOptions, coreA.URL+platformaccess.IssuePath, "", preflight, http.StatusNoContent, "")
	preflight["Access-Control-Request-Method"] = "GET"
	requestAccess(t, client, http.MethodOptions, coreA.URL+platformaccess.IssuePath, "", preflight, http.StatusForbidden, "permission_denied")
	pilotProof := raceProofIssue(t, client, []*httptest.Server{coreA, coreB}, pilot)
	noTermProof := issueAccessProof(t, client, coreA, withoutTerm, false)
	pendingProof := issueAccessProof(t, client, coreA, pending, false)
	jti := uuid.NewString()
	limited := startProofCore(t, core, keys, oldSigner, 1)
	issueAccessProof(t, client, limited, pilot, false)
	requestAccess(t, client, http.MethodPost, limited.URL+platformaccess.IssuePath, "", pilot.sdk(t, jti), http.StatusTooManyRequests, "rate_limited")
	requestAccess(t, client, http.MethodPost, coreB.URL+platformaccess.IssuePath, "", pilot.sdk(t, jti), http.StatusOK, "")
	for _, entry := range []struct {
		p      accessPilot
		status int
		code   string
	}{
		{historical, http.StatusForbidden, "permission_denied"},
		{unmapped, http.StatusServiceUnavailable, "policy_not_configured"},
		{ambiguous, http.StatusServiceUnavailable, "policy_not_configured"},
	} {
		requestAccess(t, client, http.MethodPost, coreA.URL+platformaccess.IssuePath, "", entry.p.sdk(t, uuid.NewString()), entry.status, entry.code)
	}

	pilotURL := public + pilot.path()
	first := decodeAccessState(t, requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, nil, http.StatusOK, ""))
	if !first.Active || first.Source != "own_trial" || first.Version != 1 || first.Until == nil || first.OwnUntil == nil || !first.OwnUntil.Equal(*first.Until) {
		t.Fatalf("unexpected initial TOP access: %+v", first)
	}
	checkStoredGrant(t, ctx, owner, pilot, "top_90d", 4, 90)
	checkStoredGrant(t, ctx, owner, free, "free", 1, 0)
	checkStoredGrant(t, ctx, owner, pro, "pro_14d", 1, 14)
	noTerm := decodeAccessState(t, requestAccess(t, client, http.MethodGet, public+withoutTerm.path(), noTermProof, nil, http.StatusOK, ""))
	if noTerm.Active || noTerm.Source != "none" || noTerm.Until != nil || noTerm.OwnUntil != nil {
		t.Fatalf("unconfigured product invented a term: %+v", noTerm)
	}
	requestAccess(t, client, http.MethodGet, public+pending.path(), pendingProof, nil, http.StatusNotFound, "not_found")
	requestAccess(t, client, http.MethodGet, public+"/v1/access/0/pilot", pilotProof, nil, http.StatusBadRequest, "invalid_argument")
	requestAccess(t, client, http.MethodGet, public+"/v1/access/"+pilot.selector()+"/-pilot", pilotProof, nil, http.StatusBadRequest, "invalid_argument")
	requestAccess(t, client, http.MethodGet, public+"/v1/access/"+withoutTerm.selector()+"/pilot", pilotProof, nil, http.StatusForbidden, "permission_denied")
	requestAccess(t, client, http.MethodGet, public+"/v1/access/"+pilot.selector()+"/no-policy", pilotProof, nil, http.StatusForbidden, "permission_denied")
	requestAccess(t, client, http.MethodGet, pilotURL, "invalid", nil, http.StatusUnauthorized, "unauthenticated")
	requestAccess(t, client, http.MethodOptions, pilotURL, "", map[string]string{
		"Origin": proofTestOrigin, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "Authorization",
	}, http.StatusNoContent, "")
	requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, map[string]string{"Origin": "https://foreign.amocrm.ru"}, http.StatusForbidden, "permission_denied")
	scope := platformaccess.Scope{
		Account: pilot.selector(), Product: pilot.product, Integration: pilot.integration.ID,
		Installation: pilot.installation, Generation: 1, AuthorizationSequence: 1,
	}
	for _, mutate := range []func(*platformaccess.Scope){
		func(s *platformaccess.Scope) { s.Integration = uuid.New() },
		func(s *platformaccess.Scope) { s.Installation = uuid.New() },
		func(s *platformaccess.Scope) { s.Generation, s.AuthorizationSequence = 2, 2 },
		func(s *platformaccess.Scope) { s.AuthorizationSequence = 2 },
	} {
		foreign := scope
		mutate(&foreign)
		signed, err := oldSigner.Issue(foreign, time.Now())
		checkAccessTest(t, err)
		requestAccess(t, client, http.MethodGet, pilotURL, signed.Proof, nil, http.StatusForbidden, "permission_denied")
	}
	expiredProof, err := oldSigner.Issue(scope, time.Now().Add(-platformaccess.PilotTTL))
	checkAccessTest(t, err)
	requestAccess(t, client, http.MethodGet, pilotURL, expiredProof.Proof, nil, http.StatusUnauthorized, "unauthenticated")

	coreA.Close()
	coreB.Close()
	limited.Close()
	requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, map[string]string{"Origin": proofTestOrigin}, http.StatusOK, "")
	pilot.save(t, ctx, oauthStore)
	requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, nil, http.StatusOK, "")
	commitStarted := time.Now()
	deliver(pilot, 2)
	observed := time.Now()
	requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, nil, http.StatusForbidden, "permission_denied")
	t.Logf("local reauthorization delivery and first denial: %s including mTLS receipt; denial request after receipt: %s", time.Since(commitStarted), time.Since(observed))

	coreNext := startProofCore(t, core, keys, newSigner, 100)
	reauthed := issueAccessProof(t, client, coreNext, pilot, true)
	current := decodeAccessState(t, requestAccess(t, client, http.MethodGet, pilotURL, reauthed, nil, http.StatusOK, ""))
	if !current.Active || current.Until == nil || !current.Until.Equal(*first.Until) || current.Account != first.Account || current.Product != first.Product {
		t.Fatalf("reauthorization changed account/product term: %+v", current)
	}
	operator := integrations.NewStore(core, keys)
	uninstall, err := operator.Apply(ctx, integrations.Command{
		Action: "uninstall", Actor: "p04-test", Code: pilot.product, InstallationID: pilot.installation,
	})
	checkAccessTest(t, err)
	deliver(pilot, 3)
	requestAccess(t, client, http.MethodGet, pilotURL, reauthed, nil, http.StatusForbidden, "permission_denied")
	tx, err := core.Begin(ctx)
	checkAccessTest(t, err)
	installation, err := corelifecycle.LockInstallation(ctx, tx, pilot.installation)
	checkAccessTest(t, err)
	checkAccessTest(t, journal.FinishUninstall(ctx, tx, installation, uninstall.UninstallAttempt))
	checkAccessTest(t, tx.Commit(ctx))
	deliver(pilot, 4)
	pilot.save(t, ctx, oauthStore)
	deliver(pilot, 5)
	reinstalled := issueAccessProof(t, client, coreNext, pilot, false)
	coreNext.Close()
	current = decodeAccessState(t, requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusOK, ""))
	if !current.Active || current.Until == nil || !current.Until.Equal(*first.Until) {
		t.Fatalf("reinstall changed TOP term: %+v", current)
	}
	checkStoredGrant(t, ctx, owner, pilot, "top_90d", 4, 90)

	stop()
	public, _, _, stopNext := startAccessPlatform(t, ctx, map[string]string{"new": newPub})
	defer stopNext()
	pilotURL = public + pilot.path()
	requestAccess(t, client, http.MethodGet, pilotURL, pilotProof, nil, http.StatusUnauthorized, "unauthenticated")
	requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusOK, "")
	var expiry time.Time
	checkAccessTest(t, owner.QueryRow(ctx, `UPDATE platform_lifecycle.terms SET valid_until=clock_timestamp()+interval '1500 milliseconds'
		WHERE account_id=$1 AND product_id=$2 RETURNING valid_until`, first.Account, first.Product).Scan(&expiry))
	soon := decodeAccessState(t, requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusOK, ""))
	if !soon.Active {
		t.Fatal("access was inactive before its deadline")
	}
	// No state is written between these reads, and no worker is running.
	<-time.After(max(0, time.Until(expiry)) + 25*time.Millisecond)
	elapsed := decodeAccessState(t, requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusOK, ""))
	if elapsed.Active || elapsed.Until == nil || !elapsed.Until.Equal(expiry) || elapsed.Source != "own_trial" || elapsed.OwnUntil == nil || !elapsed.OwnUntil.Equal(expiry) {
		t.Fatalf("expired term lost its provenance: %+v", elapsed)
	}

	accessPool, err := storage.Open(ctx, os.Getenv("PLATFORM_TEST_ACCESS_DSN"), storage.Access, 1)
	checkAccessTest(t, err)
	defer accessPool.Close()
	if _, err := accessPool.Exec(ctx, "SELECT * FROM platform_lifecycle.inbox"); err == nil {
		t.Fatal("Platform access role can read the lifecycle inbox")
	}
	tx, err = owner.Begin(ctx)
	checkAccessTest(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "LOCK TABLE platform_lifecycle.terms IN ACCESS EXCLUSIVE MODE")
	checkAccessTest(t, err)
	requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusServiceUnavailable, "unavailable")
	checkAccessTest(t, tx.Rollback(ctx))
	requestAccess(t, client, http.MethodGet, pilotURL, reinstalled, nil, http.StatusOK, "")
}

type accessPilot struct {
	account        int64
	product        string
	client, secret string
	integration    oauth.Integration
	installation   uuid.UUID
}

func newAccessPilot(t *testing.T, ctx context.Context, store *oauth.Store, product string, account int64) accessPilot {
	t.Helper()
	p := accessPilot{account: account, product: product, client: uuid.NewString(), secret: "test-" + uuid.NewString()}
	var err error
	p.integration, err = store.EnsureIntegration(ctx, oauth.IntegrationInput{
		Code: product, ClientID: p.client, ClientSecret: p.secret, RedirectURI: "https://core.example.test/oauth/amocrm/callback", WebhookEvents: []string{},
	})
	checkAccessTest(t, err)
	return p
}

func (p *accessPilot) save(t *testing.T, ctx context.Context, store *oauth.Store) {
	t.Helper()
	result, err := store.SaveInstallation(ctx, p.integration, oauth.Account{ID: p.account}, "proof.amocrm.ru",
		oauth.Token{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresIn: 3600})
	checkAccessTest(t, err)
	p.installation = result.ID
}

func (p accessPilot) selector() string { return strconv.FormatInt(p.account, 10) }
func (p accessPilot) path() string     { return "/v1/access/" + p.selector() + "/" + p.product }

func (p accessPilot) sdk(t *testing.T, jti string) map[string]string {
	t.Helper()
	now := time.Now().Unix()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": proofTestOrigin, "aud": "https://core.example.test", "jti": jti, "iat": now, "nbf": now,
		"exp": now + 90, "account_id": p.account, "user_id": 71, "client_uuid": p.client,
	}).SignedString([]byte(p.secret))
	checkAccessTest(t, err)
	return map[string]string{"Origin": proofTestOrigin, "X-Auth-Token": signed}
}

func startProofCore(t *testing.T, pool *pgxpool.Pool, keys *cryptox.KeyRing, signer *platformaccess.Signer, burst int) *httptest.Server {
	t.Helper()
	auth, err := widgetauth.NewAuthenticator(widgetauth.NewStore(pool), keys)
	checkAccessTest(t, err)
	limiter, err := widgetlimit.New(widgetlimit.Config{
		IntegrationRate: 0.1, IntegrationBurst: burst, InstallationRate: 0.1, InstallationBurst: burst,
		InactiveTTL: time.Hour, MaxEntries: 256,
	}, nil)
	checkAccessTest(t, err)
	cors := widgetcors.Middleware(widgetcors.NewPostgresAuthorizer(pool))
	router := chi.NewRouter()
	router.Use(httpmiddleware.RequestID)
	registerPlatformAccessProof(router, corelifecycle.NewStore(pool), signer, 2*time.Second,
		func(consume bool, next http.Handler) http.Handler {
			return protectWidgetRoute(auth, limiter, cors, consume, next)
		}, cors)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func makeAccessKey(t *testing.T, kid string) (*platformaccess.Signer, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	checkAccessTest(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	checkAccessTest(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(pub)
	checkAccessTest(t, err)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.pem")
	public := filepath.Join(dir, "public.pem")
	checkAccessTest(t, os.WriteFile(secret, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600))
	checkAccessTest(t, os.WriteFile(public, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0644))
	signer, err := platformaccess.LoadSigner(kid, secret)
	checkAccessTest(t, err)
	return signer, public
}

func startAccessPlatform(t *testing.T, ctx context.Context, keys map[string]string) (string, string, string, func()) {
	t.Helper()
	keyConfig, err := json.Marshal(keys)
	checkAccessTest(t, err)
	certs := filepath.Join(os.Getenv("PLATFORM_TEST_CERTS_DIR"), "platform")
	command := exec.CommandContext(ctx, os.Getenv("PLATFORM_TEST_BINARY"))
	command.Env = append(os.Environ(),
		"PLATFORM_ACCESS_DATABASE_URL="+os.Getenv("PLATFORM_TEST_ACCESS_DSN"),
		"PLATFORM_JOBS_DATABASE_URL="+os.Getenv("PLATFORM_TEST_JOBS_DSN"),
		"PLATFORM_TLS_CERT_FILE="+filepath.Join(certs, "tls.crt"),
		"PLATFORM_TLS_KEY_FILE="+filepath.Join(certs, "tls.key"),
		"PLATFORM_TLS_CLIENT_CA_FILE="+filepath.Join(certs, "ca.crt"),
		"PLATFORM_ACCESS_PROOF_PUBLIC_KEYS="+string(keyConfig), "PLATFORM_ACCESS_PILOT_ORIGIN="+proofTestOrigin,
		"PLATFORM_WORKERS=0", "PLATFORM_PUBLIC_HTTP_ADDRESS=127.0.0.1:0", "PLATFORM_PRIVATE_HTTP_ADDRESS=127.0.0.1:0",
		"PLATFORM_MANAGEMENT_HTTP_ADDRESS=127.0.0.1:0")
	pipe, err := command.StderrPipe()
	checkAccessTest(t, err)
	checkAccessTest(t, command.Start())
	done := make(chan error, 1)
	listening := make(chan [3]string, 1)
	go func() {
		scan := bufio.NewScanner(pipe)
		var messages []string
		for scan.Scan() {
			line := scan.Text()
			if strings.Contains(line, "Platform listening") {
				var addresses [3]string
				for i, label := range []string{"public=", "private=", "management="} {
					_, suffix, _ := strings.Cut(line, label)
					addresses[i], _, _ = strings.Cut(suffix, " ")
				}
				listening <- addresses
			} else if len(messages) < 10 {
				messages = append(messages, line)
			}
		}
		if err := command.Wait(); err != nil {
			done <- fmt.Errorf("Platform process: %w (%s)", err, strings.Join(messages, "\n"))
		} else {
			done <- nil
		}
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = command.Process.Signal(os.Interrupt)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(stop)
	select {
	case addresses := <-listening:
		return "http://" + addresses[0], "https://localhost:" + strings.TrimPrefix(addresses[1], "127.0.0.1:"), "http://" + addresses[2], stop
	case err := <-done:
		stopped = true
		t.Fatalf("Platform exited before listeners opened: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for Platform listeners")
	}
	return "", "", "", stop
}

func deliverAccessEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deliver jobs.Handler, id uuid.UUID, sequence int64) {
	t.Helper()
	var jobID uuid.UUID
	checkAccessTest(t, pool.QueryRow(ctx, `SELECT j.id FROM jobs j JOIN platform_lifecycle_ledger e ON j.resource_id=e.event_id::text
		WHERE e.installation_id=$1 AND e.sequence=$2 AND j.type=$3 ORDER BY j.created_at DESC LIMIT 1`,
		id, sequence, corelifecycle.JobType).Scan(&jobID))
	job, err := jobs.NewStore(pool).GetForInstallation(ctx, jobID, id)
	checkAccessTest(t, err)
	_, err = deliver(ctx, job)
	checkAccessTest(t, err)
}

func issueAccessProof(t *testing.T, client *http.Client, server *httptest.Server, p accessPilot, bearer bool) string {
	t.Helper()
	headers := p.sdk(t, uuid.NewString())
	proof := ""
	if bearer {
		proof = headers["X-Auth-Token"]
		delete(headers, "X-Auth-Token")
	}
	issued := requestAccess(t, client, http.MethodPost, server.URL+platformaccess.IssuePath+"?product_code=foreign", proof, headers, http.StatusOK, "")
	return decodeIssuedProof(t, issued)
}

func raceProofIssue(t *testing.T, client *http.Client, cores []*httptest.Server, pilot accessPilot) string {
	t.Helper()
	jti := uuid.NewString()
	headers := pilot.sdk(t, jti)
	start := make(chan struct{})
	results := make(chan accessTestReply, len(cores))
	for _, core := range cores {
		go func() {
			<-start
			results <- accessRequest(client, http.MethodPost, core.URL+platformaccess.IssuePath, "", headers)
		}()
	}
	close(start)
	var issued string
	denied := 0
	for range cores {
		reply := <-results
		switch reply.status {
		case http.StatusOK:
			issued = decodeIssuedProof(t, checkAccessResponse(t, reply, http.StatusOK, ""))
		case http.StatusUnauthorized:
			checkAccessResponse(t, reply, http.StatusUnauthorized, "unauthenticated")
			denied++
		default:
			t.Fatalf("concurrent issuance returned %d: %s (%v)", reply.status, reply.body, reply.err)
		}
	}
	if issued == "" || denied != len(cores)-1 {
		t.Fatal("concurrent SDK token claims did not result in exactly one issuance")
	}
	requestAccess(t, client, http.MethodPost, cores[0].URL+platformaccess.IssuePath, "", headers, http.StatusUnauthorized, "unauthenticated")
	return issued
}

func decodeIssuedProof(t *testing.T, body []byte) string {
	t.Helper()
	var issued platformaccess.Issued
	checkAccessTest(t, json.Unmarshal(body, &issued))
	if issued.Proof == "" || issued.ExpiresAt.Before(time.Now()) || issued.ExpiresAt.After(time.Now().Add(platformaccess.PilotTTL)) {
		t.Fatal("Core issued a token outside pilot TTL")
	}
	return issued.Proof
}

func checkStoredGrant(t *testing.T, ctx context.Context, owner *pgxpool.Pool, p accessPilot, rule string, version, days int) {
	t.Helper()
	var gotRule, evidence string
	var gotVersion int
	var grantTime, recordedTime, until time.Time
	checkAccessTest(t, owner.QueryRow(ctx, `
		SELECT t.policy_rule,t.policy_version,t.policy_evidence,t.granted_at,h.occurred_at,t.valid_until
		FROM platform_lifecycle.terms t JOIN platform_lifecycle.account_products p USING(account_id,product_id)
		JOIN platform_lifecycle.history h ON h.event_id=t.grant_event_id AND h.kind='installed' WHERE p.installation_id=$1`,
		p.installation).Scan(&gotRule, &gotVersion, &evidence, &grantTime, &recordedTime, &until))
	expected := grantTime.UTC().AddDate(0, 0, days)
	if rule == "free" {
		expected = grantTime.UTC().AddDate(10, 0, 0)
	}
	if gotRule != rule || gotVersion != version || evidence == "" || !grantTime.Equal(recordedTime) || !expected.Equal(until) {
		t.Fatalf("invalid initial term evidence: %s v%d %q %s %s %s", gotRule, gotVersion, evidence, grantTime, recordedTime, until)
	}
}

type accessTestReply struct {
	status int
	header http.Header
	body   []byte
	err    error
}

func accessRequest(client *http.Client, method, url, bearer string, headers map[string]string) accessTestReply {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return accessTestReply{err: err}
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := client.Do(req)
	if err != nil {
		return accessTestReply{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return accessTestReply{status: response.StatusCode, header: response.Header, body: body, err: err}
}

func requestAccess(t *testing.T, client *http.Client, method, url, bearer string, headers map[string]string, status int, code string) []byte {
	t.Helper()
	reply := accessRequest(client, method, url, bearer, headers)
	if headers["Origin"] == proofTestOrigin && status != http.StatusForbidden &&
		reply.header.Get("Access-Control-Allow-Origin") != proofTestOrigin {
		t.Fatalf("pilot CORS origin missing for %s %s (%d)", method, url, reply.status)
	}
	return checkAccessResponse(t, reply, status, code)
}

func checkAccessResponse(t *testing.T, reply accessTestReply, status int, code string) []byte {
	t.Helper()
	checkAccessTest(t, reply.err)
	if reply.status != status {
		t.Fatalf("wanted HTTP %d, got %d: %s", status, reply.status, reply.body)
	}
	if code != "" {
		var response struct {
			Error struct {
				Code       string `json:"code"`
				Message    string `json:"message"`
				RequestID  string `json:"request_id"`
				Retryable  bool   `json:"retryable"`
				RetryAfter int    `json:"retry_after_seconds"`
			} `json:"error"`
		}
		checkAccessTest(t, json.Unmarshal(reply.body, &response))
		e := response.Error
		_, err := uuid.Parse(e.RequestID)
		checkAccessTest(t, err)
		if e.Code != code || e.Message == "" || e.RequestID != reply.header.Get("X-Request-ID") {
			t.Fatalf("unexpected P01 error envelope: %s %v", reply.body, reply.header)
		}
		retry := status == http.StatusTooManyRequests || code == "unavailable"
		if e.Retryable != retry || (retry && (e.RetryAfter < 1 || reply.header.Get("Retry-After") != strconv.Itoa(e.RetryAfter))) {
			t.Fatalf("incorrect retry metadata: %s %v", reply.body, reply.header)
		}
	}
	if status != http.StatusOK || reply.header.Get("X-Request-ID") != "" {
		if reply.header.Get("Cache-Control") != "no-store" {
			t.Fatal("access response did not prohibit caching")
		}
	}
	return reply.body
}

type testedAccessState struct {
	Account  uuid.UUID  `json:"account_id"`
	Product  uuid.UUID  `json:"product_id"`
	Until    *time.Time `json:"valid_until"`
	OwnUntil *time.Time `json:"own_until"`
	Source   string     `json:"source"`
	Active   bool       `json:"active"`
	Version  int        `json:"entitlement_version"`
}

func decodeAccessState(t *testing.T, body []byte) testedAccessState {
	t.Helper()
	var response testedAccessState
	checkAccessTest(t, json.Unmarshal(body, &response))
	return response
}

func checkAccessTest(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
