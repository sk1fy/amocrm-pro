package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

type companyTokens struct{ access amocrm.AccessToken }

func (f companyTokens) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	return f.access, nil
}
func (f companyTokens) RefreshIfCurrent(context.Context, amocrm.AccessToken) (amocrm.AccessToken, error) {
	return f.access, nil
}
func (f companyTokens) MarkReauthRequired(context.Context, uuid.UUID, int64) error { return nil }

type companyTransport func(*http.Request) (*http.Response, error)

func (f companyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCompanyCRMFullTLSOwnerAndProviderChain(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	for _, failure := range []string{"", "wrong_account", "rate_limited", "timeout", "binding_unavailable"} {
		t.Run("cold_"+failure, func(t *testing.T) {
			b := companycrm.Binding{AccountID: 99, InstallationID: uuid.New(), IntegrationID: uuid.New(), Revision: "synthetic-r1"}
			ctx := t.Context()
			if _, err := pool.Exec(ctx, `INSERT INTO integrations(id,code,client_id,client_secret_ciphertext,redirect_uri,status) VALUES($1,$2,$3,'fixture','https://fixture.test/oauth','active')`, b.IntegrationID, "company-chain-"+failure, "fixture-client-"+failure); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO installations(id,integration_id,account_id,account_domain,status) VALUES($1,$2,$3,'company.amocrm.test','active')`, b.InstallationID, b.IntegrationID, b.AccountID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO oauth_credentials(installation_id,access_token_ciphertext,refresh_token_ciphertext,expires_at,token_version,key_version) VALUES($1,'fixture','fixture',now()+interval '1 hour',1,1)`, b.InstallationID); err != nil {
				t.Fatal(err)
			}
			if failure == "binding_unavailable" {
				if _, err := pool.Exec(ctx, `DELETE FROM oauth_credentials WHERE installation_id=$1`, b.InstallationID); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int64
			tokens := companyTokens{access: amocrm.AccessToken{InstallationID: b.InstallationID, IntegrationID: b.IntegrationID, AccountID: b.AccountID, AccountDomain: "company.amocrm.ru", Value: "synthetic-only", TokenVersion: 1}}
			provider := amocrm.NewClient(&http.Client{Transport: companyTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Host != "company.amocrm.ru" || r.Header.Get("Authorization") != "Bearer synthetic-only" {
					t.Errorf("unexpected synthetic provider request")
				}
				body := ""
				switch r.URL.Path {
				case "/api/v4/account":
					if failure == "timeout" {
						return nil, context.DeadlineExceeded
					}
					if failure == "rate_limited" {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"5"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
					}
					account := 99
					if failure == "wrong_account" {
						account = 100
					}
					body = fmt.Sprintf(`{"id":%d}`, account)
				case "/api/v4/users":
					if failure != "" {
						t.Error("cold failure reached dataset read")
					}
					body = `{"_embedded":{"users":[{"id":7,"name":"Synthetic","rights":{"is_active":null}}]}}`
				default:
					t.Errorf("unexpected provider path %s", r.URL.Path)
					body = `{}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}, tokens)
			dir, clientTLS := companyIdentityFiles(t)
			cfg := companycrm.Config{Address: "127.0.0.1:0", IdentityDir: dir, Binding: b}
			server, err := companyCRMServer(pool, provider, tokens, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("constructor contacted provider")
			}
			listener, err := net.Listen("tcp", cfg.Address)
			if err != nil {
				t.Fatal(err)
			}
			run, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() {
				done <- serveCompanyCRM(run, server, listener, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
			}()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			transport := &http.Transport{TLSClientConfig: clientTLS}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			read := func() companycrm.Response[[]companycrm.StaffMember] {
				response, err := client.Get("https://" + listener.Addr().String() + "/v1/company-crm/staff")
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if id, err := uuid.Parse(response.Header.Get("X-Request-ID")); err != nil || id == uuid.Nil {
					t.Fatalf("missing response correlation ID: %q", response.Header.Get("X-Request-ID"))
				}
				want := http.StatusOK
				if failure != "" {
					want = http.StatusServiceUnavailable
				}
				if response.StatusCode != want {
					t.Fatal(response.StatusCode)
				}
				var value companycrm.Response[[]companycrm.StaffMember]
				if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			first := read()
			if failure != "" {
				if first.Data != nil || first.Source.Complete || first.Source.State != "error" {
					t.Fatalf("cold failure=%+v", first)
				}
				before := calls.Load()
				read()
				if calls.Load() != before {
					t.Fatal("cold failure bypassed cooldown")
				}
				return
			}
			if first.Source.State != "fresh" || !first.Source.Complete || first.Data == nil || len(*first.Data) != 1 || (*first.Data)[0].Activity != "unknown" {
				t.Fatalf("chain=%+v", first)
			}
			before := calls.Load()
			if _, err := pool.Exec(ctx, `UPDATE installations SET status='disabled' WHERE id=$1`, b.InstallationID); err != nil {
				t.Fatal(err)
			}
			stale := read()
			if stale.Source.State != "stale" || stale.Source.ErrorCode != "binding_unavailable" || stale.Source.BindingRevision != b.Revision || stale.Data == nil || calls.Load() != before {
				t.Fatalf("revoked=%+v provider calls before=%d after=%d", stale, before, calls.Load())
			}
		})
	}
}

func companyIdentityFiles(t *testing.T) (string, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	write := func(name, kind string, der []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.crt", "CERTIFICATE", der)
	cert := func(id string, usage x509.ExtKeyUsage) tls.Certificate {
		pub, private, _ := ed25519.GenerateKey(rand.Reader)
		u, _ := url.Parse("spiffe://amocrm-pro/" + id)
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{"localhost"}, URIs: []*url.URL{u}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	}
	server := cert("core", x509.ExtKeyUsageServerAuth)
	write("tls.crt", "CERTIFICATE", server.Certificate[0])
	private, err := x509.MarshalPKCS8PrivateKey(server.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write("tls.key", "PRIVATE KEY", private)
	return dir, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{cert("admin-company-directory", x509.ExtKeyUsageClientAuth)}}
}
