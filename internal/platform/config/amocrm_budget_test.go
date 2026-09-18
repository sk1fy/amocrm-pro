package config

import (
	"testing"
	"time"
)

func workerEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://example.invalid/db")
	t.Setenv("ENCRYPTION_KEYS", "1:"+developmentEncryptionKey)
	t.Setenv("APP_ENV", "development")
	t.Setenv("PUBLIC_BASE_URL", "https://backend.example.test")
}

func TestAmoCRMBudgetDefaultsMatchTheAmoCRMBaseline(t *testing.T) {
	workerEnvironment(t)
	worker, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	budget := worker.AmoCRMBudget
	if budget.PairRPS != 7 || budget.AccountRPS != 50 || budget.PairBurst != 1 || budget.AccountBurst != 1 {
		t.Fatalf("unexpected budget defaults: %+v", budget)
	}
	if budget.Replicas != 1 || budget.MaxWait != 5*time.Second {
		t.Fatalf("unexpected overload defaults: %+v", budget)
	}
	if budget.PairWaiters != 64 || budget.AccountWaiters != 512 || budget.ProcessWaiters != 4096 {
		t.Fatalf("unexpected queue defaults: %+v", budget)
	}
	if len(budget.PairRPSByAccount) != 0 {
		t.Fatalf("unexpected default overrides: %+v", budget.PairRPSByAccount)
	}
}

func TestAmoCRMBudgetReadsPerAccountOverrides(t *testing.T) {
	workerEnvironment(t)
	t.Setenv("AMOCRM_BUDGET_ACCOUNT_PAIR_RPS", "1234567:20, 7654321:15")
	worker, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	overrides := worker.AmoCRMBudget.PairRPSByAccount
	if overrides[1234567] != 20 || overrides[7654321] != 15 || len(overrides) != 2 {
		t.Fatalf("unexpected overrides: %+v", overrides)
	}
}

func TestAmoCRMBudgetRejectsInvalidSettings(t *testing.T) {
	for name, value := range map[string]string{
		"AMOCRM_BUDGET_PAIR_RPS":         "0",
		"AMOCRM_BUDGET_ACCOUNT_RPS":      "120",
		"AMOCRM_BUDGET_PAIR_BURST":       "0",
		"AMOCRM_BUDGET_MAX_WAIT":         "10m",
		"AMOCRM_BUDGET_REPLICAS":         "0",
		"AMOCRM_BUDGET_PAIR_WAITERS":     "0",
		"AMOCRM_BUDGET_INACTIVE_TTL":     "2h",
		"AMOCRM_BUDGET_ACCOUNT_PAIR_RPS": "1234567:80",
	} {
		t.Run(name, func(t *testing.T) {
			workerEnvironment(t)
			t.Setenv(name, value)
			if _, err := LoadWorker(); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		})
	}
	for _, malformed := range []string{"1234567", "abc:20", "0:20", "1234567:20,1234567:15", "1234567:0"} {
		t.Run("override "+malformed, func(t *testing.T) {
			workerEnvironment(t)
			t.Setenv("AMOCRM_BUDGET_ACCOUNT_PAIR_RPS", malformed)
			if _, err := LoadWorker(); err == nil {
				t.Fatalf("accepted malformed override %q", malformed)
			}
		})
	}
}

// Waiter limits must nest: a single pair may never book the whole account or
// process capacity.
func TestAmoCRMBudgetRejectsInvertedWaiterLimits(t *testing.T) {
	workerEnvironment(t)
	t.Setenv("AMOCRM_BUDGET_PAIR_WAITERS", "600")
	if _, err := LoadWorker(); err == nil {
		t.Fatal("accepted a pair queue larger than the account queue")
	}
}

func TestAmoCRMBudgetRejectsMultipleOutboundOwners(t *testing.T) {
	workerEnvironment(t)
	t.Setenv("AMOCRM_BUDGET_REPLICAS", "2")
	if _, err := LoadWorker(); err == nil {
		t.Fatal("accepted multiple process-local outbound owners")
	}
}
