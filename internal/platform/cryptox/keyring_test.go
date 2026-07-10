package cryptox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
)

func TestParseKeyRingAndRotate(t *testing.T) {
	t.Parallel()

	keyOne := bytes.Repeat([]byte{0x11}, KeySize)
	keyTwo := bytes.Repeat([]byte{0x22}, KeySize)
	encoded := "1:" + base64.StdEncoding.EncodeToString(keyOne) +
		",2:" + base64.StdEncoding.EncodeToString(keyTwo)

	ring, err := ParseKeyRing(encoded, 2)
	if err != nil {
		t.Fatalf("ParseKeyRing() error = %v", err)
	}
	if got := ring.ActiveVersion(); got != 2 {
		t.Fatalf("ActiveVersion() = %d, want 2", got)
	}

	aad := IntegrationSecretAAD(uuid.MustParse("38f1842b-082a-4ea5-a88e-9982706b85ad"))
	ciphertext, version, err := ring.Seal([]byte("client secret"), aad)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if version != 2 {
		t.Fatalf("Seal() version = %d, want 2", version)
	}
	if bytes.Contains(ciphertext, []byte("client secret")) {
		t.Fatal("Seal() returned plaintext in ciphertext")
	}

	plaintext, err := ring.Open(version, ciphertext, aad)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := string(plaintext); got != "client secret" {
		t.Fatalf("Open() = %q, want %q", got, "client secret")
	}
}

func TestParseKeyRingRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	validKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, KeySize))
	tests := map[string]struct {
		encoded string
		active  int
	}{
		"empty":                  {encoded: "", active: 1},
		"surrounding whitespace": {encoded: " 1:" + validKey, active: 1},
		"empty entry":            {encoded: "1:" + validKey + ",", active: 1},
		"missing separator":      {encoded: "1" + validKey, active: 1},
		"extra separator":        {encoded: "1:" + validKey + ":", active: 1},
		"zero version":           {encoded: "0:" + validKey, active: 1},
		"negative version":       {encoded: "-1:" + validKey, active: 1},
		"leading zero version":   {encoded: "01:" + validKey, active: 1},
		"duplicate version":      {encoded: "1:" + validKey + ",1:" + validKey, active: 1},
		"invalid base64":         {encoded: "1:not-base64", active: 1},
		"wrong key size":         {encoded: "1:" + base64.StdEncoding.EncodeToString([]byte("short")), active: 1},
		"missing active version": {encoded: "1:" + validKey, active: 2},
		"invalid active version": {encoded: "1:" + validKey, active: 0},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseKeyRing(test.encoded, test.active)
			if !errors.Is(err, ErrInvalidKeyRing) {
				t.Fatalf("ParseKeyRing() error = %v, want ErrInvalidKeyRing", err)
			}
		})
	}
}

func TestNewKeyRingCopiesInputKeys(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte{0x33}, KeySize)
	ring, err := NewKeyRing(map[int][]byte{1: key}, 1)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	clear(key)

	ciphertext, version, err := ring.Seal([]byte("secret"), nil)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	plaintext, err := ring.Open(version, ciphertext, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("Open() = %q, want secret", plaintext)
	}
}

func TestOpenRejectsWrongAADTamperingAndVersions(t *testing.T) {
	t.Parallel()

	ring, err := NewKeyRing(map[int][]byte{7: bytes.Repeat([]byte{0x44}, KeySize)}, 7)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	ciphertext, version, err := ring.Seal([]byte("secret"), []byte("correct aad"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	if _, err := ring.Open(version, ciphertext, []byte("wrong aad")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("Open(wrong AAD) error = %v, want ErrInvalidCiphertext", err)
	}

	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 1
	if _, err := ring.Open(version, tampered, []byte("correct aad")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("Open(tampered) error = %v, want ErrInvalidCiphertext", err)
	}
	if _, err := ring.Open(version, ciphertext[:8], []byte("correct aad")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("Open(short) error = %v, want ErrInvalidCiphertext", err)
	}
	if _, err := ring.Open(99, ciphertext, []byte("correct aad")); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("Open(unknown version) error = %v, want ErrUnknownKeyVersion", err)
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	t.Parallel()

	ring, err := NewKeyRing(map[int][]byte{1: bytes.Repeat([]byte{0x55}, KeySize)}, 1)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	first, _, err := ring.Seal([]byte("same"), nil)
	if err != nil {
		t.Fatalf("first Seal() error = %v", err)
	}
	second, _, err := ring.Seal([]byte("same"), nil)
	if err != nil {
		t.Fatalf("second Seal() error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("Seal() reused a nonce")
	}
}

func TestSealPropagatesRandomSourceFailure(t *testing.T) {
	t.Parallel()

	ring, err := newKeyRing(
		map[int][]byte{1: bytes.Repeat([]byte{0x66}, KeySize)},
		1,
		io.LimitReader(bytes.NewReader(nil), 0),
	)
	if err != nil {
		t.Fatalf("newKeyRing() error = %v", err)
	}
	if _, _, err := ring.Seal([]byte("secret"), nil); !errors.Is(err, io.EOF) {
		t.Fatalf("Seal() error = %v, want io.EOF", err)
	}
}
