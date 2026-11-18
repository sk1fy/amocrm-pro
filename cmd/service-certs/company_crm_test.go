package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformDevelopmentCertsIncludeNarrowCompanyIdentities(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	if err := generate(dir, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"admin-company-directory", "platform-company-crm"} {
		body, err := os.ReadFile(filepath.Join(dir, id, "tls.crt"))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(body)
		if block == nil {
			t.Fatal("missing certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://amocrm-pro/"+id || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			t.Fatalf("identity=%s certificate=%+v", id, cert)
		}
	}
	if err := generate(dir, true); err == nil {
		t.Fatal("existing identity directory overwritten")
	}
}
