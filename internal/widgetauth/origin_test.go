package widgetauth

import "testing"

func TestAudienceForRedirectURI(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  string
	}{
		"path and query removed": {
			value: "https://External.Example:443/oauth/callback?source=amo#done",
			want:  "https://external.example",
		},
		"nondefault port preserved": {
			value: "http://localhost:8080/callback",
			want:  "http://localhost:8080",
		},
		"IPv6": {
			value: "http://[::1]:8080/callback",
			want:  "http://[::1]:8080",
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := AudienceForRedirectURI(test.value)
			if err != nil {
				t.Fatalf("AudienceForRedirectURI() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("AudienceForRedirectURI() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIssuerForAccountDomain(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  string
	}{
		"bare domain":  {value: "Subdomain.AmoCRM.ru", want: "https://subdomain.amocrm.ru"},
		"HTTPS origin": {value: "https://Subdomain.AmoCRM.ru:443/", want: "https://subdomain.amocrm.ru"},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := IssuerForAccountDomain(test.value)
			if err != nil {
				t.Fatalf("IssuerForAccountDomain() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("IssuerForAccountDomain() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOriginNormalizationRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	redirects := []string{
		"",
		" relative/callback",
		"/relative/callback",
		"ftp://external.example/callback",
		"https://user:pass@external.example/callback",
		"https://external.example:/callback",
	}
	for _, value := range redirects {
		value := value
		t.Run("redirect "+value, func(t *testing.T) {
			t.Parallel()
			if _, err := AudienceForRedirectURI(value); err == nil {
				t.Fatalf("AudienceForRedirectURI(%q) error = nil", value)
			}
		})
	}

	domains := []string{
		"",
		" subdomain.amocrm.ru",
		"http://subdomain.amocrm.ru",
		"https://subdomain.amocrm.ru/path",
		"https://subdomain.amocrm.ru?query=yes",
		"https://user@subdomain.amocrm.ru",
	}
	for _, value := range domains {
		value := value
		t.Run("domain "+value, func(t *testing.T) {
			t.Parallel()
			if _, err := IssuerForAccountDomain(value); err == nil {
				t.Fatalf("IssuerForAccountDomain(%q) error = nil", value)
			}
		})
	}
}
