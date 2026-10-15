package taskcontrol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"testing"
)

func TestAssertionWireFixture(t *testing.T) {
	raw, e := os.ReadFile("testdata/assertion.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Assertion string    `json:"assertion"`
		Claims    Assertion `json:"claims"`
	}
	if e = json.Unmarshal(raw, &f); e != nil {
		t.Fatal(e)
	}
	signed, e := SignAssertion("fixture", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)), f.Claims)
	if e != nil || signed != f.Assertion {
		t.Fatal("wire signature changed", e)
	}
}
