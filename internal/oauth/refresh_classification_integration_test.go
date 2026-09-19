package oauth

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
	"testing"
)

func TestOnlyDefinitiveRefreshFailureMarksReauth(t *testing.T) {
	for _, tc := range []struct {
		kind amocrm.ErrorKind
		want string
	}{{amocrm.ErrorInvalidGrant, "reauth_required"}, {amocrm.ErrorUnauthorized, "reauth_required"}, {amocrm.ErrorValidation, "active"}, {amocrm.ErrorTemporary, "active"}, {amocrm.ErrorRateLimited, "active"}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			pool := testkit.Postgres(t)
			testkit.Reset(t, pool)
			keys := oauthTestKeyRing(t)
			id := oauthTestInstallation(t, pool, keys, -1)
			gateway := &oauthTestGateway{refresh: func(context.Context, string, string, string, string, string) (Token, error) {
				return Token{}, &amocrm.APIError{Kind: tc.kind}
			}}
			_, err := NewTokenProvider(pool, keys, gateway).Token(t.Context(), id)
			if err == nil {
				t.Fatal("missing failure")
			}
			var state string
			if err := pool.QueryRow(t.Context(), `SELECT status FROM installations WHERE id=$1`, id).Scan(&state); err != nil || state != tc.want {
				t.Fatalf("state=%s want %s err=%v", state, tc.want, err)
			}
		})
	}
}
