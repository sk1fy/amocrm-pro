package sanitize

import (
	"strings"
	"unicode/utf8"
)

// Text returns valid PostgreSQL UTF-8 text without NUL bytes and bounded by
// maxBytes without splitting a multi-byte rune.
func Text(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxBytes {
		return value
	}
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if end+size > maxBytes {
			break
		}
		end += size
	}
	return value[:end]
}
