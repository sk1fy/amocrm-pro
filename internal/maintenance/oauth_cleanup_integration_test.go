package maintenance

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestCleanupOAuthStatesExpiryMarginAndBounds(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	integrationID, _ := cleanupTenant(t, pool)
	ctx := context.Background()
	for _, state := range []struct {
		name     string
		expiry   string
		consumed bool
	}{
		{"expired-unused", "-2 hours", false},
		{"expired-consumed", "-2 hours", true},
		{"margin-unused", "-30 minutes", false},
		{"margin-consumed", "-30 minutes", true},
		{"live-unused", "1 hour", false},
		{"live-consumed", "1 hour", true},
	} {
		hash := sha256.Sum256([]byte(state.name))
		_, err := pool.Exec(ctx, `INSERT INTO oauth_states
			(integration_id, state_hash, created_at, expires_at, consumed_at)
			VALUES ($1, $2, now()-interval '4 hours', now()+$3::interval,
			CASE WHEN $4 THEN now()-interval '3 hours' ELSE NULL END)`,
			integrationID, hash[:], state.expiry, state.consumed)
		if err != nil {
			t.Fatal(err)
		}
	}
	policy := testPolicy(1, 1)
	policy.SafetyMargin = time.Hour
	store := NewStore(pool)
	for i := 0; i < 2; i++ {
		result, err := store.Cleanup(ctx, policy)
		if err != nil {
			t.Fatal(err)
		}
		if result.OAuthStates != 1 || !result.OAuthStatesLimitReached {
			t.Fatalf("bounded OAuth cleanup = %+v", result)
		}
	}
	result, err := store.Cleanup(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result.OAuthStates != 0 || result.OAuthStatesLimitReached {
		t.Fatalf("completed OAuth cleanup = %+v", result)
	}
	var remaining, expired int
	if err := pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE expires_at < now()-interval '1 hour')
		FROM oauth_states`).Scan(&remaining, &expired); err != nil {
		t.Fatal(err)
	}
	if remaining != 4 || expired != 0 {
		t.Fatalf("remaining/expired OAuth states = %d/%d", remaining, expired)
	}
}
