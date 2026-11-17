package platformaccess

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
)

var keyID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func LoadSigner(kid, path string) (*Signer, error) {
	if !keyID.MatchString(kid) || path == "" {
		return nil, errors.New("Core access proof requires a valid key ID and private key file")
	}
	block, err := readPEM(path, "PRIVATE KEY")
	if err != nil {
		return nil, errors.New("could not load Core access proof PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("Core access proof private key must be Ed25519")
	}
	return &Signer{kid: kid, key: key}, nil
}

// LoadVerifier configures a local, bounded map of key IDs to SPKI PEM files.
func LoadVerifier(config string) (*Verifier, error) {
	if len(config) == 0 || len(config) > 16384 {
		return nil, errors.New("expected Platform access proof public key map")
	}
	members, err := jsonMembers(json.NewDecoder(strings.NewReader(config)), 16)
	if err != nil || len(members) == 0 {
		return nil, errors.New("expected Platform access proof public key map")
	}
	keys := make(map[string]ed25519.PublicKey, len(members))
	for kid, value := range members {
		var path string
		if !keyID.MatchString(kid) || json.Unmarshal(value, &path) != nil || path == "" {
			return nil, errors.New("invalid Platform access proof public key entry")
		}
		block, err := readPEM(path, "PUBLIC KEY")
		if err != nil {
			return nil, errors.New("could not load Platform access proof SPKI public key")
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		key, ok := parsed.(ed25519.PublicKey)
		if err != nil || !ok {
			return nil, errors.New("Platform access proof public key must be Ed25519")
		}
		keys[kid] = key
	}
	return &Verifier{keys: keys}, nil
}

func readPEM(path, kind string) (*pem.Block, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxPEMBytes = 8192
	raw, err := io.ReadAll(io.LimitReader(file, maxPEMBytes+1))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	block, rest := pem.Decode(trimmed)
	if len(raw) > maxPEMBytes || !bytes.HasPrefix(trimmed, []byte("-----BEGIN "+kind+"-----")) ||
		block == nil || block.Type != kind || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid access proof key PEM")
	}
	return block, nil
}
