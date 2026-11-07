package config

import "testing"

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
