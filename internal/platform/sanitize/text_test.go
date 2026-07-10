package sanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextPreservesUTF8BoundaryAndRemovesNUL(t *testing.T) {
	value := strings.Repeat("a", 3999) + "€" + "\x00tail"
	result := Text(value, 4000)
	if !utf8.ValidString(result) {
		t.Fatal("result is not valid UTF-8")
	}
	if len(result) != 3999 {
		t.Fatalf("unexpected byte length: %d", len(result))
	}
	if strings.ContainsRune(result, '\x00') {
		t.Fatal("result contains NUL")
	}
}
