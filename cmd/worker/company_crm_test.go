package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

func TestCompanyCRMDisabledWithoutConfiguration(t *testing.T) {
	s, err := companyCRMServer(t.Context(), nil, nil, nil, companycrm.Config{})
	if err != nil || s != nil {
		t.Fatal(s, err)
	}
}
func TestCompanyCRMConstructorDoesNotReadDependencies(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://unused:unused@127.0.0.1:1/unused_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	dir, _ := companyIdentityFiles(t)
	provider := amocrm.NewClient(&http.Client{Transport: companyTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("constructor contacted CRM")
		return nil, nil
	})}, companyTokens{})
	server, err := companyCRMServer(t.Context(), pool, provider, companyTokens{}, companycrm.Config{Address: "127.0.0.1:0", IdentityDir: dir})
	if err != nil || server == nil {
		t.Fatalf("constructor depends on unavailable owner DB: server=%v err=%v", server, err)
	}
}

func TestCompanyCRMRejectsInvalidTLSAtConstruction(t *testing.T) {
	server, err := companyCRMServer(t.Context(), nil, nil, nil, companycrm.Config{Address: "127.0.0.1:0", IdentityDir: t.TempDir()})
	if err == nil || server != nil {
		t.Fatalf("invalid TLS accepted: server=%v err=%v", server, err)
	}
}
func TestCompanyCRMWorkerServesOnlyMutualTLS(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
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
	certificate := func(id string, usage x509.ExtKeyUsage) tls.Certificate {
		pub, private, _ := ed25519.GenerateKey(rand.Reader)
		uri, _ := url.Parse("spiffe://amocrm-pro/" + id)
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{"localhost"}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: companycrm.Router(&companycrm.Service{}), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate("core", x509.ExtKeyUsageServerAuth)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}, ReadHeaderTimeout: time.Second, ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serveCompanyCRM(ctx, server, listener, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	address := "https://" + listener.Addr().String() + "/v1/company-crm/staff"
	for _, tc := range []struct {
		name  string
		cert  *tls.Certificate
		tlsOK bool
	}{
		{"missing", nil, false},
		{"server_eku", ptr(certificate("admin-company-directory", x509.ExtKeyUsageServerAuth)), false},
		{"old_team_os", ptr(certificate("team-os", x509.ExtKeyUsageClientAuth)), true},
		{"platform_cannot_read_staff", ptr(certificate("platform-company-crm", x509.ExtKeyUsageClientAuth)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
			if tc.cert != nil {
				cfg.Certificates = []tls.Certificate{*tc.cert}
			}
			transport := &http.Transport{TLSClientConfig: cfg}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			response, err := client.Get(address)
			if !tc.tlsOK {
				if err == nil {
					response.Body.Close()
					t.Fatal("untrusted client reached HTTP")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Fatal(response.StatusCode)
			}
		})
	}
	plain := &http.Client{Timeout: time.Second}
	response, err := plain.Get("http://" + listener.Addr().String() + "/v1/company-crm/staff")
	if err == nil {
		defer response.Body.Close()
		if response.StatusCode == 200 {
			t.Fatal("plaintext source exposed")
		}
	}
}
func ptr[T any](v T) *T { return &v }
