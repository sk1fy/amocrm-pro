package productplatform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPlatformProcess(t *testing.T) {
	public := os.Getenv("PLATFORM_TEST_PUBLIC_URL")
	if public == "" {
		t.Skip("run make platform-test for Platform process checks")
	}
	private := os.Getenv("PLATFORM_TEST_PRIVATE_URL")
	management := os.Getenv("PLATFORM_TEST_MANAGEMENT_URL")
	certs := os.Getenv("PLATFORM_TEST_CERTS_DIR")

	client := &http.Client{Timeout: 3 * time.Second}
	expectStatus(t, client, public+"/live", http.StatusOK)
	expectStatus(t, client, public+"/ready", http.StatusOK)
	expectStatus(t, client, public+"/metrics", http.StatusNotFound)
	expectStatus(t, client, public+coreEventsPath, http.StatusNotFound)
	expectStatus(t, client, public+managerPath+"installations", http.StatusNotFound)
	expectStatus(t, client, public+"/v1/access/42/example", http.StatusNotFound)

	core := tlsClient(t, certs, "core")
	manager := tlsClient(t, certs, "manager-bff")
	checkPrivateRoutes(t, private, core, manager, http.StatusNotFound)
	if resp, err := tlsClient(t, certs, "").Get(private + coreEventsPath); err == nil {
		resp.Body.Close()
		t.Fatal("private listener accepted a request without a client certificate")
	}
	plain := strings.Replace(private, "https://", "http://", 1)
	if resp, err := client.Get(plain + managerPath + "installations"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("private listener served plaintext request: %d", resp.StatusCode)
		}
	}

	serverFiles := filepath.Join(certs, "platform")
	tlsCfg, err := PrivateTLS(filepath.Join(serverFiles, "tls.crt"), filepath.Join(serverFiles, "tls.key"), filepath.Join(serverFiles, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	fixture := httptest.NewUnstartedServer(PrivateHandler(fixtureHandler, fixtureHandler))
	fixture.TLS = tlsCfg
	fixture.StartTLS()
	defer fixture.Close()
	fixtureURL := "https://" + net.JoinHostPort("localhost", strconv.Itoa(fixture.Listener.Addr().(*net.TCPAddr).Port))
	checkPrivateRoutes(t, fixtureURL, core, manager, http.StatusNoContent)

	testIndependentReadiness(t, client, public, management)
}

func testIndependentReadiness(t *testing.T, client *http.Client, public, management string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := pgxpool.New(ctx, os.Getenv("PLATFORM_TEST_OWNER_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	awaitMetric(ctx, t, client, management, "platform_queue_snapshot_ok 1")
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "LOCK TABLE platform_jobs.jobs IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	awaitMetric(ctx, t, client, management, "platform_queue_snapshot_ok 0")
	awaitMetric(ctx, t, client, management, "platform_queue_database_errors_total{operation=\"recover\"} 1")
	expectStatus(t, client, public+"/ready", http.StatusOK)
	expectStatus(t, client, management+"/ready", http.StatusOK)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	awaitMetric(ctx, t, client, management, "platform_queue_snapshot_ok 1")

	if _, err := owner.Exec(ctx, "REVOKE SELECT ON platform_access.runtime_probe FROM platform_access"); err != nil {
		t.Fatal(err)
	}
	const grant = "GRANT SELECT ON platform_access.runtime_probe TO platform_access"
	defer func() {
		grantCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := owner.Exec(grantCtx, grant); err != nil {
			t.Error(err)
		}
	}()
	expectStatus(t, client, public+"/live", http.StatusOK)
	expectStatus(t, client, public+"/ready", http.StatusServiceUnavailable)
	awaitMetric(ctx, t, client, management, "platform_queue_snapshot_ok 1")
	if _, err := owner.Exec(ctx, grant); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, client, public+"/ready", http.StatusOK)

}

func checkPrivateRoutes(t *testing.T, base string, core, manager *http.Client, allowed int) {
	t.Helper()
	expectStatus(t, core, base+coreEventsPath, allowed)
	expectStatus(t, manager, base+managerPath+"installations", allowed)
	expectStatus(t, manager, base+coreEventsPath, http.StatusForbidden)
	expectStatus(t, core, base+managerPath+"installations", http.StatusForbidden)
}

func tlsClient(t *testing.T, certs, identity string) *http.Client {
	t.Helper()
	rootPEM, err := os.ReadFile(filepath.Join(certs, "platform", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatal("read test client CA")
	}
	cfg := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	if identity != "" {
		bundle := filepath.Join(certs, identity)
		pair, err := tls.LoadX509KeyPair(filepath.Join(bundle, "tls.crt"), filepath.Join(bundle, "tls.key"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	transport := &http.Transport{TLSClientConfig: cfg}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Timeout: 3 * time.Second, Transport: transport}
}

func expectStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("%s: status %d, want %d", url, response.StatusCode, want)
	}
}

func awaitMetric(ctx context.Context, t *testing.T, client *http.Client, base, value string) {
	t.Helper()
	for ctx.Err() == nil {
		response, err := client.Get(base + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode == http.StatusOK && strings.Contains(string(body), value+"\n") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("management metrics never reported %s: %v", value, ctx.Err())
}
