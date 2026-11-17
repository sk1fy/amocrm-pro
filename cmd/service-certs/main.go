// service-certs generates ephemeral development identities for the local pilot.
// Certificates last 30 days and include only service/localhost DNS names plus
// the api/worker aliases used by the development Compose stack. It is not a
// production CA: do not add production DNS/IP SANs, extend TTL, reuse the
// output on a new host, or disable TLS verification to make a placement work.
// Production identities come from the environment CA with the static RPC
// hostname in SAN; rotation is docs/runbooks/secrets-rotation.md.
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

	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
)

func main() {
	if buildinfo.PrintVersion(os.Args, os.Stdout) {
		return
	}
	args := os.Args[1:]
	platform := len(args) > 0 && args[0] == "--platform"
	if platform {
		args = args[1:]
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: service-certs [--platform] OUTPUT_DIRECTORY")
		fmt.Fprintln(os.Stderr, "development only: 30-day certs for service/localhost DNS (api/worker aliases); refuses an existing output directory")
		fmt.Fprintln(os.Stderr, "not a production CA; do not extend TTL or disable TLS verification for a new placement")
		fmt.Fprintln(os.Stderr, "production identities: environment CA + docs/runbooks/secrets-rotation.md")
		os.Exit(2)
	}
	if err := generate(args[0], platform); err != nil {
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

func generate(dir string, platform bool) error {
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
	caName := "Activity development CA"
	if platform {
		caName = "Platform development CA"
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: caName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * 24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPub, caKey)
	if err != nil {
		return err
	}
	identities := []string{"core", "gateway", "activity", "crm-events"}
	if platform {
		identities = []string{"platform", "core", "manager-bff"}
	}
	for i, identity := range identities {
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
		usages := []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		if platform && identity == "platform" {
			usages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		} else if platform {
			usages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: identity},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, DNSNames: names,
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages}
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
	if platform {
		fmt.Fprintln(os.Stderr, "service-certs: wrote Platform development identities (30-day localhost).")
		return nil
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
	if err := writeExclusive(filepath.Join(dir, "gateway", "delegation.key"), "PRIVATE KEY", der); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "service-certs: wrote development identities (30-day localhost). Not a production CA; do not use these certificates after a host/DNS change.")
	return nil
}
