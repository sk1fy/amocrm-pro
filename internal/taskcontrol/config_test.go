package taskcontrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestWidgetJWTPolicy(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TASK_CONTROL_HTTP_ADDRESS", ":8087")
	t.Setenv("TASK_CONTROL_TLS_PROXY", "true")
	t.Setenv("TASK_CONTROL_SERVICE_KEYS", `{"test":"abcdefghijklmnopqrstuvwxyz012345"}`)
	t.Setenv("TASK_CONTROL_ASSERTION_KEY_ID", "test")
	t.Setenv("TASK_CONTROL_ASSERTION_PRIVATE_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("WIDGET_JWT_LEEWAY", "")
	t.Setenv("WIDGET_JWT_MAX_LIFETIME", "")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.JWTMaxLifetime != 30*time.Minute || c.JWTLeeway != 5*time.Second {
		t.Fatalf("unexpected defaults: %v %v", c.JWTMaxLifetime, c.JWTLeeway)
	}
	for _, value := range []string{"garbage", "0s", "-1m", "61m"} {
		t.Setenv("WIDGET_JWT_MAX_LIFETIME", value)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	t.Setenv("WIDGET_JWT_MAX_LIFETIME", "30m")
	t.Setenv("WIDGET_JWT_LEEWAY", "61s")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("accepted unbounded clock skew")
	}
}
