package amocrm

import "testing"

func TestAccountBaseURL(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"example.amocrm.ru", true},
		{"https://team.amocrm.com/", true},
		{"tenant.kommo.com", true},
		{"https://amocrm.ru", false},
		{"http://example.amocrm.ru", false},
		{"https://127.0.0.1", false},
		{"https://example.amocrm.ru:8443", false},
		{"https://example.amocrm.ru/path", false},
		{"https://example.amocrm.ru.evil.test", false},
	}
	for _, test := range tests {
		_, err := AccountBaseURL(test.input)
		if (err == nil) != test.valid {
			t.Errorf("AccountBaseURL(%q) error=%v, want valid=%t", test.input, err, test.valid)
		}
	}
}
