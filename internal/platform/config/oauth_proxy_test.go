package config

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v2"
)

func TestOAuthComposeSettingsReachAPI(t *testing.T) {
	for _, file := range []string{"docker-compose.yml", "docker-compose.activity.yml"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile("../../../" + file)
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Services map[string]struct{ Environment map[string]string }
			}
			if err := yaml.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{
				"OAUTH_TRUSTED_PROXY_CIDRS":      "${OAUTH_TRUSTED_PROXY_CIDRS:-}",
				"OAUTH_IP_RATE_PER_SECOND":       "${OAUTH_IP_RATE_PER_SECOND:-5}",
				"OAUTH_IP_BURST":                 "${OAUTH_IP_BURST:-20}",
				"OAUTH_IDENTITY_RATE_PER_SECOND": "${OAUTH_IDENTITY_RATE_PER_SECOND:-2}",
				"OAUTH_IDENTITY_BURST":           "${OAUTH_IDENTITY_BURST:-8}",
				"OAUTH_LIMITER_INACTIVE_TTL":     "${OAUTH_LIMITER_INACTIVE_TTL:-10m}",
				"OAUTH_LIMITER_MAX_ENTRIES":      "${OAUTH_LIMITER_MAX_ENTRIES:-10000}",
			} {
				if got := config.Services["api"].Environment[key]; got != value {
					t.Errorf("API %s=%q, want %q", key, got, value)
				}
			}
		})
	}
}

func TestOAuthProxyConfiguration(t *testing.T) {
	for _, tt := range []struct {
		value   string
		want    int
		invalid bool
	}{{"", 0, false}, {"192.0.2.0/24,2001:db8::/64", 2, false}, {"::ffff:192.0.2.0/120", 1, false}, {"192.0.2.1", 0, true}, {"proxy.example.test/24", 0, true}} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
			t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
			t.Setenv("APP_ENV", "development")
			t.Setenv("OAUTH_TRUSTED_PROXY_CIDRS", tt.value)
			cfg, err := LoadAPI()
			if tt.invalid {
				if err == nil {
					t.Fatal("invalid proxy CIDR accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.OAuthTrustedProxyCIDRs) != tt.want {
				t.Fatalf("trusted prefixes=%v", cfg.OAuthTrustedProxyCIDRs)
			}
			if tt.value == "::ffff:192.0.2.0/120" && cfg.OAuthTrustedProxyCIDRs[0].String() != "192.0.2.0/24" {
				t.Fatalf("mapped prefix=%v", cfg.OAuthTrustedProxyCIDRs)
			}
		})
	}
}
