package admincrm

import (
	"os"
	"strings"
	"testing"
)

func TestProductionSourceOmitsSecretSubstrings(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source := strings.ToLower(string(body))
		for _, forbidden := range []string{"ciphertext", "webhook_key", "email", "access_token", "refresh_token"} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s mentions %s", name, forbidden)
			}
		}
	}
}
