package distribution

import (
	"testing"
	"time"
)

func TestConnectionRuntimeJWTAndTLSConfiguration(t *testing.T) {
	t.Setenv("DISTRIBUTION_HTTP_ADDRESS", ":8084")
	t.Setenv("DISTRIBUTION_SERVICE_KEYS", `{"service":"fixture-private-key-at-least32characters"}`)
	t.Setenv("DISTRIBUTION_TEAMOS_KEY_ID", "service")
	t.Setenv("DISTRIBUTION_TEAMOS_URL", "https://team-os.example.test")
	t.Setenv("DISTRIBUTION_TLS_PROXY", "true")
	t.Setenv("WIDGET_JWT_LEEWAY", "10s")
	t.Setenv("WIDGET_JWT_MAX_LIFETIME", "2m")
	c, e := LoadConfig()
	if e != nil || c.JWTLeeway != 10*time.Second || c.JWTMaxLifetime != 2*time.Minute {
		t.Fatal("JWT policy not shared", c.JWTLeeway, c.JWTMaxLifetime, e)
	}
	for _, v := range []struct{ key, value string }{{"DISTRIBUTION_TLS_PROXY", "false"}, {"DISTRIBUTION_TEAMOS_URL", "http://team-os.example.test"}, {"WIDGET_JWT_MAX_LIFETIME", "2h"}, {"WIDGET_JWT_LEEWAY", "2m"}, {"WIDGET_JWT_MAX_LIFETIME", "1s"}} {
		t.Run(v.key+v.value, func(t *testing.T) {
			t.Setenv(v.key, v.value)
			if _, e := LoadConfig(); e == nil {
				t.Fatal("insecure config accepted")
			}
		})
	}
}
