package widgetauth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestThirtyMinuteDisposablePolicyStillRejectsInvalidClaims(t *testing.T) {
	f := newAuthFixture(t)
	auth, err := NewAuthenticator(f.repository, f.authenticator.secrets,
		WithClock(func() time.Time { return testNow }), WithMaxLifetime(30*time.Minute), WithLeeway(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	claims := cloneClaims(f.claims)
	claims["iat"] = testNow.Add(-time.Second).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = testNow.Add(30*time.Minute - time.Second).Unix()
	raw := signClaims(t, claims, f.secret, jwt.SigningMethodHS256)
	if _, err := auth.Verify(context.Background(), raw); err != nil {
		t.Fatalf("amoCRM 1800-second token rejected: %v", err)
	}
	for name, change := range map[string]func(jwt.MapClaims){
		"longer lifetime": func(c jwt.MapClaims) { c["exp"] = testNow.Add(30 * time.Minute).Unix() },
		"wrong audience":  func(c jwt.MapClaims) { c["aud"] = "https://ya.ru" },
		"wrong issuer":    func(c jwt.MapClaims) { c["iss"] = "https://other.amocrm.ru" },
		"expired": func(c jwt.MapClaims) {
			c["iat"] = testNow.Add(-31 * time.Minute).Unix()
			c["nbf"] = c["iat"]
			c["exp"] = testNow.Add(-time.Minute).Unix()
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := cloneClaims(claims)
			change(c)
			if _, err := auth.Verify(context.Background(), signClaims(t, c, f.secret, jwt.SigningMethodHS256)); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	if _, err := auth.Verify(context.Background(), signClaims(t, claims, []byte("wrong-secret"), jwt.SigningMethodHS256)); err == nil {
		t.Fatal("invalid signature accepted")
	}
}
