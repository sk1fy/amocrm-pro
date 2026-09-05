package widgetlimit

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

func testConfig() Config {
	return Config{IntegrationRate: 1, IntegrationBurst: 3, InstallationRate: 1,
		InstallationBurst: 1, InactiveTTL: 10 * time.Second, MaxEntries: 100}
}

func TestHierarchicalLimitsPreserveOtherInstallationBudget(t *testing.T) {
	now := time.Now()
	l, err := newLimiter(testConfig(), nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	i, a, b, c := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if ok, _ := l.allow(i, a); !ok {
		t.Fatal("initial request denied")
	}
	for range 10 {
		if ok, retry := l.allow(i, a); ok || retry != time.Second {
			t.Fatalf("exhausted installation: allowed=%t retry=%s", ok, retry)
		}
	}
	for _, installation := range []uuid.UUID{b, c} {
		if ok, _ := l.allow(i, installation); !ok {
			t.Fatal("rejected traffic consumed integration budget")
		}
	}
	if ok, _ := l.allow(i, uuid.New()); ok {
		t.Fatal("aggregate integration limit bypassed by new installation")
	}
	if ok, _ := l.allow(uuid.New(), uuid.New()); !ok {
		t.Fatal("integration A exhausted integration B")
	}
	now = now.Add(time.Second)
	if ok, _ := l.allow(i, a); !ok {
		t.Fatal("tokens did not refill")
	}
}

func TestConcurrentRequestsCannotExceedBurst(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.InstallationBurst = 3
	l, err := newLimiter(config, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	i, a := uuid.New(), uuid.New()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.allow(i, a); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 3 {
		t.Fatalf("concurrent accepted=%d", allowed.Load())
	}
}

func TestCacheBoundAndIdleEvictionDoNotResetActiveBudgets(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.MaxEntries = 1
	registry := prometheus.NewRegistry()
	l, err := newLimiter(config, registry, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	i, a := uuid.New(), uuid.New()
	if ok, _ := l.allow(i, a); !ok {
		t.Fatal("initial request denied")
	}
	for range 20 {
		if ok, _ := l.allow(uuid.New(), uuid.New()); ok {
			t.Fatal("cache limit bypassed")
		}
	}
	if len(l.integrations) != 1 || len(l.installations) != 1 {
		t.Fatal("unbounded cache growth")
	}
	if ok, _ := l.allow(i, a); ok {
		t.Fatal("saturated cache evicted active budget")
	}
	if got := testutil.ToFloat64(l.decisions.WithLabelValues("capacity", "rejected")); got != 20 {
		t.Fatalf("capacity rejects=%g", got)
	}
	now = now.Add(config.InactiveTTL + time.Second)
	if ok, _ := l.allow(uuid.New(), uuid.New()); !ok {
		t.Fatal("idle entries were not evicted")
	}
	if got := testutil.ToFloat64(l.entries.WithLabelValues("installation")); got != 1 {
		t.Fatalf("entry gauge=%g", got)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), i.String()) || strings.Contains(label.GetValue(), a.String()) {
					t.Fatal("unbounded identity label")
				}
			}
		}
	}
}

func TestMiddlewareRequiresVerifiedPrincipalAndReturnsRetryContract(t *testing.T) {
	now := time.Now()
	l, err := newLimiter(testConfig(), nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodPost, "/widget/action", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || len(l.integrations) != 0 {
		t.Fatal("unverified request allocated quota")
	}
	p := widgetauth.Principal{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	req = req.WithContext(widgetauth.ContextWithPrincipal(req.Context(), p))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatal("first request rejected")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" || w.Header().Get("Cache-Control") != "no-store" ||
		w.Header().Get("Content-Type") != "application/json" || w.Body.String() != "{\"error\":{\"code\":\"rate_limited\"}}\n" || calls != 1 {
		t.Fatalf("rate rejection: code=%d headers=%v body=%s calls=%d", w.Code, w.Header(), w.Body.String(), calls)
	}
}

func TestConfigurationRejectsUnsafeRatesAndEarlyEviction(t *testing.T) {
	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		config := testConfig()
		config.IntegrationRate = rate
		if _, err := New(config, nil); err == nil {
			t.Fatal("invalid rate accepted")
		}
	}
	for _, alter := range []func(*Config){
		func(c *Config) { c.InactiveTTL = time.Second },
		func(c *Config) { c.MaxEntries = 0 },
		func(c *Config) { c.InstallationBurst = 0 },
	} {
		config := testConfig()
		alter(&config)
		if _, err := New(config, nil); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
