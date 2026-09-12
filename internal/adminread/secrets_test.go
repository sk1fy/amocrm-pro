package adminread

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
		if strings.Contains(source, "ciphertext") {
			t.Errorf("%s mentions ciphertext", name)
		}
		if strings.Contains(string(body), "webhook_key") {
			t.Errorf("%s mentions webhook_key", name)
		}
	}
}
