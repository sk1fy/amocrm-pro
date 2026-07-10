// Package cryptox provides authenticated encryption for secrets stored in the
// database. A KeyRing is immutable after construction and safe for concurrent
// use.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const (
	KeySize = 32
)

var (
	ErrInvalidKeyRing    = errors.New("invalid encryption key ring")
	ErrUnknownKeyVersion = errors.New("unknown encryption key version")
	ErrInvalidCiphertext = errors.New("invalid ciphertext")
)

// KeyRing holds AES-256-GCM keys indexed by a positive integer version.
// Ciphertexts contain a random nonce prefix; the key version is stored by the
// caller next to the ciphertext.
type KeyRing struct {
	keys          map[int]cipher.AEAD
	activeVersion int
	random        io.Reader
}

// ParseKeyRing parses a comma-separated key ring in the form
// "version:base64,version:base64". Versions must be canonical positive decimal
// integers and every decoded key must contain exactly 32 bytes.
func ParseKeyRing(encoded string, activeVersion int) (*KeyRing, error) {
	if encoded == "" || encoded != strings.TrimSpace(encoded) {
		return nil, fmt.Errorf("%w: value is empty or has surrounding whitespace", ErrInvalidKeyRing)
	}

	entries := strings.Split(encoded, ",")
	keys := make(map[int][]byte, len(entries))
	for index, entry := range entries {
		if entry == "" || strings.Count(entry, ":") != 1 {
			return nil, fmt.Errorf("%w: entry %d must have version:base64 form", ErrInvalidKeyRing, index+1)
		}

		parts := strings.SplitN(entry, ":", 2)
		version, err := strconv.Atoi(parts[0])
		if err != nil || version <= 0 || strconv.Itoa(version) != parts[0] {
			return nil, fmt.Errorf("%w: entry %d has a non-canonical positive version", ErrInvalidKeyRing, index+1)
		}
		if _, exists := keys[version]; exists {
			return nil, fmt.Errorf("%w: duplicate version %d", ErrInvalidKeyRing, version)
		}

		key, err := base64.StdEncoding.Strict().DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d is not strict standard base64: %v", ErrInvalidKeyRing, index+1, err)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("%w: version %d must decode to %d bytes", ErrInvalidKeyRing, version, KeySize)
		}
		keys[version] = key
	}

	return newKeyRing(keys, activeVersion, rand.Reader)
}

// NewKeyRing constructs a ring from raw keys. Input key bytes are copied.
func NewKeyRing(keys map[int][]byte, activeVersion int) (*KeyRing, error) {
	return newKeyRing(keys, activeVersion, rand.Reader)
}

func newKeyRing(keys map[int][]byte, activeVersion int, random io.Reader) (*KeyRing, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: at least one key is required", ErrInvalidKeyRing)
	}
	if activeVersion <= 0 {
		return nil, fmt.Errorf("%w: active version must be positive", ErrInvalidKeyRing)
	}
	if random == nil {
		return nil, fmt.Errorf("%w: random source is nil", ErrInvalidKeyRing)
	}

	aeads := make(map[int]cipher.AEAD, len(keys))
	for version, sourceKey := range keys {
		if version <= 0 {
			return nil, fmt.Errorf("%w: version %d must be positive", ErrInvalidKeyRing, version)
		}
		if len(sourceKey) != KeySize {
			return nil, fmt.Errorf("%w: version %d must contain %d bytes", ErrInvalidKeyRing, version, KeySize)
		}

		key := append([]byte(nil), sourceKey...)
		block, err := aes.NewCipher(key)
		clear(key)
		if err != nil {
			return nil, fmt.Errorf("%w: initialize version %d: %v", ErrInvalidKeyRing, version, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("%w: initialize GCM version %d: %v", ErrInvalidKeyRing, version, err)
		}
		aeads[version] = aead
	}
	if _, exists := aeads[activeVersion]; !exists {
		return nil, fmt.Errorf("%w: active version %d is missing", ErrInvalidKeyRing, activeVersion)
	}

	return &KeyRing{keys: aeads, activeVersion: activeVersion, random: random}, nil
}

// ActiveVersion returns the version Seal currently uses.
func (r *KeyRing) ActiveVersion() int {
	if r == nil {
		return 0
	}
	return r.activeVersion
}

// Seal encrypts plaintext with the active key and authenticates additionalData.
// The returned ciphertext is nonce || GCM-sealed-data.
func (r *KeyRing) Seal(plaintext, additionalData []byte) ([]byte, int, error) {
	if r == nil {
		return nil, 0, fmt.Errorf("%w: key ring is nil", ErrInvalidKeyRing)
	}
	aead, exists := r.keys[r.activeVersion]
	if !exists {
		return nil, 0, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, r.activeVersion)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(r.random, nonce); err != nil {
		return nil, 0, fmt.Errorf("generate encryption nonce: %w", err)
	}

	sealed := make([]byte, len(nonce), len(nonce)+len(plaintext)+aead.Overhead())
	copy(sealed, nonce)
	sealed = aead.Seal(sealed, nonce, plaintext, additionalData)
	return sealed, r.activeVersion, nil
}

// Open authenticates and decrypts nonce-prefixed ciphertext with the requested
// key version.
func (r *KeyRing) Open(keyVersion int, ciphertext, additionalData []byte) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: key ring is nil", ErrInvalidKeyRing)
	}
	aead, exists := r.keys[keyVersion]
	if !exists {
		return nil, fmt.Errorf("%w: %d", ErrUnknownKeyVersion, keyVersion)
	}
	minimumLength := aead.NonceSize() + aead.Overhead()
	if len(ciphertext) < minimumLength {
		return nil, fmt.Errorf("%w: got %d bytes, need at least %d", ErrInvalidCiphertext, len(ciphertext), minimumLength)
	}

	nonce := ciphertext[:aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, ciphertext[aead.NonceSize():], additionalData)
	if err != nil {
		return nil, fmt.Errorf("%w: authentication failed", ErrInvalidCiphertext)
	}
	return plaintext, nil
}

// IntegrationSecretAAD binds an encrypted amoCRM client secret to its stable
// integration row. The same helper must be used for both Seal and Open.
func IntegrationSecretAAD(integrationID uuid.UUID) []byte {
	return []byte("amocrm:integration:" + integrationID.String() + ":client-secret:v1")
}

func InstallationOAuthAAD(installationID uuid.UUID) []byte {
	return []byte("amocrm:installation:" + installationID.String() + ":oauth-credentials:v1")
}

func InstallationWebhookKeyAAD(installationID uuid.UUID) []byte {
	return []byte("amocrm:installation:" + installationID.String() + ":webhook-key:v1")
}
