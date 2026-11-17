package platformaccess

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProofValidation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := Signer{kid: "pilot", key: private}
	verifier := Verifier{keys: map[string]ed25519.PublicKey{"pilot": public}}
	at := time.Unix(1712345678, 0)
	scope := Scope{
		Account: "9007199254740993", Product: "pilot", Integration: uuid.New(), Installation: uuid.New(),
		Generation: 2, AuthorizationSequence: 3,
	}
	issued, err := signer.Issue(scope, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{at, issued.ExpiresAt.Add(-time.Nanosecond)} {
		got, err := verifier.Verify(issued.Proof, now)
		if err != nil || got.Scope != scope || got.ExpiresAt != issued.ExpiresAt.Unix() || got.IssuedAt != at.Unix() {
			t.Fatalf("valid proof rejected at %s: %+v, %v", now, got, err)
		}
	}
	for _, now := range []time.Time{at.Add(-time.Nanosecond), issued.ExpiresAt} {
		if _, err := verifier.Verify(issued.Proof, now); err == nil {
			t.Fatalf("proof accepted outside exclusive time window at %s", now)
		}
	}
	parts := strings.Split(issued.Proof, ".")
	protected, err := b64.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(payload) {
		t.Fatal("invalid JWT claims JSON")
	}
	testCases := []struct {
		name             string
		section          int
		old, replacement string
	}{
		{"algorithm", 0, "EdDSA", "HS256"},
		{"type", 0, "JWT", "access+jwt"},
		{"unknown key", 0, "pilot", "old"},
		{"missing key", 0, `,"kid":"pilot"`, ""},
		{"duplicate key", 0, `"kid":"pilot"`, `"kid":"pilot","kid":"pilot"`},
		{"remote key", 0, `"typ":"JWT"`, `"typ":"JWT","jku":"https://invalid.test/jwks"`},
		{"audience array", 1, `"platform-access-v1"`, `["platform-access-v1"]`},
		{"issuer", 1, `"iss":"core"`, `"iss":"platform"`},
		{"numeric account", 1, `"9007199254740993"`, `9007199254740993`},
		{"noncanonical account", 1, `"9007199254740993"`, `"09007199254740993"`},
		{"noncanonical ID", 1, scope.Installation.String(), "urn:uuid:" + scope.Installation.String()},
		{"noninteger generation", 1, `"generation":2`, `"generation":2.0`},
		{"invalid generation", 1, `"generation":2`, `"generation":0`},
		{"epoch outside wire range", 1, `"authorization_sequence":3`, `"authorization_sequence":9007199254740992`},
		{"null issue time", 1, `"iat":1712345678`, `"iat":null`},
		{"issue after not before", 1, `"iat":1712345678`, `"iat":1712345679`},
		{"not before in future", 1, `"nbf":1712345678`, `"nbf":1712345679`},
		{"too long", 1, `"exp":1712345978`, `"exp":1712345979`},
		{"extra member", 1, `"nbf":1712345678`, `"nbf":1712345678,"jti":"extra"`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			segments := []string{string(protected), string(payload)}
			updated := strings.Replace(segments[tc.section], tc.old, tc.replacement, 1)
			if updated == segments[tc.section] {
				t.Fatal("test did not alter proof")
			}
			segments[tc.section] = updated
			message := b64.EncodeToString([]byte(segments[0])) + "." + b64.EncodeToString([]byte(segments[1]))
			signature := ed25519.Sign(private, []byte(message))
			if _, err := verifier.Verify(message+"."+b64.EncodeToString(signature), at); err == nil {
				t.Fatal("malformed proof accepted")
			}
		})
	}
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := Verifier{keys: map[string]ed25519.PublicKey{"pilot": other}}
	if _, err := wrongKey.Verify(issued.Proof, at); err == nil {
		t.Fatal("proof accepted with wrong signature")
	}
}
