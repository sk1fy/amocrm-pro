// service-certs generates ephemeral development identities for the local pilot.
// Production deployments supply identities from their own CA and secret store.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: service-certs OUTPUT_DIRECTORY (development only, refuses existing keys)")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writeExclusive(path, kind string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = pem.Encode(f, &pem.Block{Type: kind, Bytes: data})
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func generate(dir string) error {
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		return fmt.Errorf("output directory must not exist: %s", dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	caPub, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Activity development CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * 24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPub, caKey)
	if err != nil {
		return err
	}
	for i, identity := range []string{"core", "gateway", "activity", "crm-events"} {
		path := filepath.Join(dir, identity)
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		uri, _ := url.Parse("spiffe://amocrm-pro/" + identity)
		names := []string{identity, "localhost"}
		if identity == "gateway" {
			names = append(names, "worker")
		}
		if identity == "core" {
			names = append(names, "api")
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: identity},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, DNSNames: names,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, pub, caKey)
		if err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		for _, file := range []struct {
			name, kind string
			value      []byte
		}{{"ca.crt", "CERTIFICATE", caDER}, {"tls.crt", "CERTIFICATE", der}, {"tls.key", "PRIVATE KEY", keyDER}} {
			if err := writeExclusive(filepath.Join(path, file.name), file.kind, file.value); err != nil {
				return err
			}
		}
	}
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(signing)
	if err != nil {
		return err
	}
	// Only the Gateway/policy process issues delegation. The CA private key is
	// deliberately not persisted or mounted in any runtime container.
	return writeExclusive(filepath.Join(dir, "gateway", "delegation.key"), "PRIVATE KEY", der)
}
