package platformaccess

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const (
	Issuer    = "core"
	Audience  = "platform-access-v1"
	PilotTTL  = 5 * time.Minute
	IssuePath = "/api/v1/widget/platform-access-proof"
)

var ErrInvalidProof = errors.New("invalid Platform access proof")

var b64 = base64.RawURLEncoding.Strict()

type Scope struct {
	Account               string    `json:"sub"`
	Product               string    `json:"product_code"`
	Integration           uuid.UUID `json:"integration_id"`
	Installation          uuid.UUID `json:"core_installation_id"`
	Generation            int64     `json:"generation"`
	AuthorizationSequence int64     `json:"authorization_sequence"`
}

type Claims struct {
	Scope
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`
}

type Issued struct {
	Proof     string    `json:"proof"`
	ExpiresAt time.Time `json:"expires_at"`
}

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type Signer struct {
	kid string
	key ed25519.PrivateKey
}

func (s *Signer) Issue(scope Scope, now time.Time) (Issued, error) {
	if !scope.valid() || now.Unix() < 0 {
		return Issued{}, ErrInvalidProof
	}
	issuedAt := now.Unix()
	expiresAt := issuedAt + int64(PilotTTL/time.Second)
	protected, err := json.Marshal(header{Algorithm: "EdDSA", Type: "JWT", KeyID: s.kid})
	if err != nil {
		return Issued{}, err
	}
	payload, err := json.Marshal(Claims{
		Scope: scope, Issuer: Issuer, Audience: Audience, IssuedAt: issuedAt,
		NotBefore: issuedAt, ExpiresAt: expiresAt,
	})
	if err != nil {
		return Issued{}, err
	}
	message := b64.EncodeToString(protected) + "." + b64.EncodeToString(payload)
	sig := ed25519.Sign(s.key, []byte(message))
	return Issued{Proof: message + "." + b64.EncodeToString(sig), ExpiresAt: time.Unix(expiresAt, 0).UTC()}, nil
}

type Verifier struct {
	keys map[string]ed25519.PublicKey
}

func (v *Verifier) Verify(proof string, now time.Time) (Claims, error) {
	const maxHeader, maxPayload = 256, 2048
	signatureSize := b64.EncodedLen(ed25519.SignatureSize)
	if len(proof) > maxHeader+maxPayload+signatureSize+2 {
		return Claims{}, ErrInvalidProof
	}
	headerPart, payloadAndSignature, ok := strings.Cut(proof, ".")
	payloadPart, signaturePart, hasSignature := strings.Cut(payloadAndSignature, ".")
	if !ok || !hasSignature || len(headerPart) > maxHeader || len(payloadPart) > maxPayload ||
		len(signaturePart) != signatureSize || strings.ContainsAny(proof, "\r\n") {
		return Claims{}, ErrInvalidProof
	}
	protected, err := b64.DecodeString(headerPart)
	if err != nil {
		return Claims{}, ErrInvalidProof
	}
	var h header
	if exactJSON(protected, &h, "alg", "typ", "kid") == nil || h.Algorithm != "EdDSA" || h.Type != "JWT" {
		return Claims{}, ErrInvalidProof
	}
	key, ok := v.keys[h.KeyID]
	if !ok {
		return Claims{}, ErrInvalidProof
	}
	sig, err := b64.DecodeString(signaturePart)
	if err != nil || !ed25519.Verify(key, []byte(headerPart+"."+payloadPart), sig) {
		return Claims{}, ErrInvalidProof
	}
	payload, err := b64.DecodeString(payloadPart)
	if err != nil {
		return Claims{}, ErrInvalidProof
	}
	var c Claims
	claims := exactJSON(payload, &c, "iss", "aud", "sub", "product_code", "integration_id", "core_installation_id",
		"generation", "authorization_sequence", "iat", "nbf", "exp")
	if claims == nil || !canonicalUUID(claims["integration_id"], c.Integration) ||
		!canonicalUUID(claims["core_installation_id"], c.Installation) ||
		c.Issuer != Issuer || c.Audience != Audience || !c.Scope.valid() ||
		c.IssuedAt < 0 || c.NotBefore < c.IssuedAt || c.ExpiresAt <= c.NotBefore ||
		c.ExpiresAt-c.IssuedAt > int64(PilotTTL/time.Second) ||
		now.Unix() < c.NotBefore || now.Unix() >= c.ExpiresAt {
		return Claims{}, ErrInvalidProof
	}
	return c, nil
}

func (s Scope) valid() bool {
	_, err := wire.AccountID(s.Account)
	return err == nil && wire.ValidProductCode(s.Product) && s.Integration != uuid.Nil &&
		s.Installation != uuid.Nil && s.Generation >= 1 && s.AuthorizationSequence >= s.Generation &&
		s.AuthorizationSequence <= wire.MaxPosition
}

// exactJSON rejects duplicate, missing, extra and null members before decoding
// their types. The proof contract uses exact member names and scalar claims.
func exactJSON(raw []byte, target any, fields ...string) map[string]json.RawMessage {
	d := json.NewDecoder(bytes.NewReader(raw))
	members, err := jsonMembers(d, len(fields))
	if err != nil || len(members) != len(fields) {
		return nil
	}
	for _, field := range fields {
		value := members[field]
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return nil
		}
	}
	if json.Unmarshal(raw, target) != nil {
		return nil
	}
	return members
}

func canonicalUUID(raw json.RawMessage, id uuid.UUID) bool {
	var text string
	return json.Unmarshal(raw, &text) == nil && text == id.String()
}

func jsonMembers(d *json.Decoder, limit int) (map[string]json.RawMessage, error) {
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, ErrInvalidProof
	}
	members := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		field, ok := token.(string)
		if err != nil || !ok || len(members) == limit {
			return nil, ErrInvalidProof
		}
		if _, duplicate := members[field]; duplicate {
			return nil, ErrInvalidProof
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, ErrInvalidProof
		}
		members[field] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrInvalidProof
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalidProof
	}
	return members, nil
}
