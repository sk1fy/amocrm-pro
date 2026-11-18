package companysource

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/companycrm"
)

func testIdentity(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey, role string) (tls.Certificate, string, string) {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse("spiffe://amocrm-pro/" + role)
	cert := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: role}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, URIs: []*url.URL{u}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, cert, ca, &k.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	pk, e := x509.MarshalECPrivateKey(k)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if e = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk}), 0600); e != nil {
		t.Fatal(e)
	}
	pair, e := tls.LoadX509KeyPair(cp, kp)
	if e != nil {
		t.Fatal(e)
	}
	return pair, cp, kp
}
func TestCompanySourceTLSAndWire(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca, _ = x509.ParseCertificate(der)
	pemCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if e = os.WriteFile(caPath, pemCA, 0600); e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pemCA)
	serverCert, _, _ := testIdentity(t, ca, key, "core")
	_, clientCert, clientKey := testIdentity(t, ca, key, "platform-company-crm")
	mode := "full"
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/company-crm/clients" || r.Method != "GET" || len(r.TLS.PeerCertificates[0].URIs) != 1 || r.TLS.PeerCertificates[0].URIs[0].String() != "spiffe://amocrm-pro/platform-company-crm" {
			t.Error("wrong private contract")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, "https://localhost/elsewhere", 302)
			return
		case "large":
			_, _ = w.Write([]byte(strings.Repeat("x", MaxBody+1)))
			return
		case "malformed":
			_, _ = w.Write([]byte("{"))
			return
		case "slow":
			<-r.Context().Done()
			return
		case "bare429":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		now := time.Now()
		until := now.Add(time.Minute)
		data := companycrm.Clients{Companies: []companycrm.Client{}, Contacts: []companycrm.Contact{}}
		response := companycrm.Response[companycrm.Clients]{Source: companycrm.Source{AccountID: 12, BindingRevision: "r1", Complete: true, State: "fresh", ObservedAt: &now, FreshUntil: &until}, Data: &data}
		switch mode {
		case "stale":
			response.Source.State = "stale"
		case "partial":
			response.Source.Complete = false
			response.Data = nil
			response.Source.State = "error"
			w.WriteHeader(503)
		case "mismatch":
			response.Source.AccountID = 13
		case "rate":
			response.Source.RetryAfter = 50
			response.Source.Complete = false
			response.Source.State = "error"
			response.Data = nil
			w.Header().Set("Retry-After", "20")
			w.WriteHeader(429)
		}
		_ = json.NewEncoder(w).Encode(response)
		if mode == "trailing" {
			_, _ = w.Write([]byte("{}"))
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	defer server.Close()
	cfg := Config{Origin: strings.Replace(server.URL, "127.0.0.1", "localhost", 1), CAFile: caPath, CertFile: clientCert, KeyFile: clientKey, AccountID: 12, Revision: "r1"}
	client, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	for _, test := range []struct {
		mode  string
		fails bool
	}{{"full", false}, {"stale", false}, {"partial", false}, {"rate", false}, {"bare429", true}, {"redirect", true}, {"large", true}, {"malformed", true}, {"trailing", true}, {"mismatch", true}} {
		t.Run(test.mode, func(t *testing.T) {
			mode = test.mode
			r, e := client.Clients(context.Background())
			if (e != nil) != test.fails {
				t.Fatalf("error=%v result=%+v", e, r)
			}
			if mode == "full" && (r.Response.Data == nil || r.Response.Data.Companies == nil || !r.Response.Source.Complete) {
				t.Fatal("empty full lost")
			}
			if mode == "partial" && (r.Response.Data != nil || r.Status != 503) {
				t.Fatal("partial lost")
			}
			if mode == "rate" && (r.RetryAt == nil || time.Until(*r.RetryAt) < 49*time.Second || r.Status != 429) {
				t.Fatal("retry metadata lost")
			}
			if mode == "bare429" && (r.RetryAt == nil || r.Status != 429) {
				t.Fatal("bare retry lost")
			}
		})
	}
	mode = "slow"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, e = client.Clients(ctx); e == nil {
		t.Fatal("deadline ignored")
	}
	cfg.Origin = "https://localhost:1"
	offline, e := New(cfg)
	if e != nil {
		t.Fatal("constructor dialed", e)
	}
	offline.Close()
	cfg.Origin = server.URL
	wrongHost, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer wrongHost.Close()
	mode = "full"
	if _, e = wrongHost.Clients(context.Background()); e == nil {
		t.Fatal("hostname not verified")
	}
	_, cfg.CertFile, cfg.KeyFile = testIdentity(t, ca, key, "manager-bff")
	if _, e = New(cfg); e == nil {
		t.Fatal("wrong client role accepted")
	}
}
func TestCompanySourceConfig(t *testing.T) {
	for _, origin := range []string{"http://localhost", "https://localhost/", "https://u:p@localhost", "https://localhost?q=1", "https://localhost#x"} {
		c := Config{Origin: origin, CAFile: "ca", CertFile: "cert", KeyFile: "key", AccountID: 1, Revision: "r"}
		if c.validate() == nil {
			t.Fatal(origin)
		}
	}
	if _, e := ParseConfig(func(string) string { return "" }); e != nil {
		t.Fatal(e)
	}
	if _, e := ParseConfig(func(k string) string {
		if k == "PLATFORM_COMPANY_SOURCE_ACCOUNT_ID" {
			return "1"
		}
		return ""
	}); e == nil {
		t.Fatal("partial configuration accepted")
	}
}
