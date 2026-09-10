package oauthlimit

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func testConfig() Config {
	return Config{IPRate: 1, IPBurst: 3, IdentityRate: 1, IdentityBurst: 1, InactiveTTL: 10 * time.Second, MaxEntries: 100}
}

func TestIdentityLimitDoesNotSpendIPBudgetOnRejection(t *testing.T) {
	now := time.Now()
	l, err := newLimiter(testConfig(), nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.allow("203.0.113.10", "c\x00known"); !ok {
		t.Fatal("initial request denied")
	}
	for range 10 {
		if ok, retry := l.allow("203.0.113.10", "c\x00known"); ok || retry != time.Second {
			t.Fatalf("exhausted identity: allowed=%t retry=%s", ok, retry)
		}
	}
	if ok, _ := l.allow("203.0.113.10", "c\x00other"); !ok {
		t.Fatal("rejected identity consumed IP budget")
	}
	if ok, _ := l.allow("203.0.113.10", "c\x00third"); !ok {
		t.Fatal("second identity on same IP denied")
	}
	if ok, _ := l.allow("203.0.113.10", "c\x00fourth"); ok {
		t.Fatal("IP burst bypassed")
	}
	if ok, _ := l.allow("203.0.113.11", "c\x00known"); ok {
		t.Fatal("identity budget is per code, not per IP")
	}
	if ok, _ := l.allow("203.0.113.11", "c\x00fresh"); !ok {
		t.Fatal("identity A exhausted identity B")
	}
}

func TestConcurrentRequestsCannotExceedBurst(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.IdentityBurst = 3
	l, err := newLimiter(config, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.allow("198.51.100.9", "c\x00same"); ok {
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
	if ok, _ := l.allow("203.0.113.10", "c\x00known"); !ok {
		t.Fatal("initial request denied")
	}
	for range 20 {
		if ok, _ := l.allow("198.51.100.1", "c\x00other"); ok {
			t.Fatal("cache limit bypassed")
		}
	}
	if len(l.ips) != 1 || len(l.identities) != 1 {
		t.Fatal("unbounded cache growth")
	}
	if ok, _ := l.allow("203.0.113.10", "c\x00known"); ok {
		t.Fatal("saturated cache evicted active budget")
	}
	if got := testutil.ToFloat64(l.decisions.WithLabelValues("capacity", "rejected")); got != 20 {
		t.Fatalf("capacity rejects=%g", got)
	}
	now = now.Add(config.InactiveTTL + time.Second)
	if ok, _ := l.allow("198.51.100.8", "c\x00fresh"); !ok {
		t.Fatal("idle entries were not evicted")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), "203.0.113.10") || strings.Contains(label.GetValue(), "known") {
					t.Fatal("unbounded identity label")
				}
			}
		}
	}
}

func TestMiddlewareReturnsIdenticalRateLimitBody(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.IPBurst = 1
	config.IdentityBurst = 1
	l, err := newLimiter(config, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	first := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/start?integration_code=known", nil)
	first.RemoteAddr = "203.0.113.10:9"
	first.Header.Set("X-Request-ID", "11111111-1111-1111-1111-111111111111")
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "11111111-1111-1111-1111-111111111111")
	handler.ServeHTTP(w, first)
	if w.Code != http.StatusNoContent {
		t.Fatal("first request rejected")
	}
	bodies := []string{}
	for _, rawURL := range []string{
		"/oauth/amocrm/start?integration_code=known",
		"/oauth/amocrm/start?integration_code=unknown",
	} {
		req := httptest.NewRequest(http.MethodGet, rawURL, nil)
		req.RemoteAddr = "203.0.113.10:9"
		rec := httptest.NewRecorder()
		rec.Header().Set("X-Request-ID", "11111111-1111-1111-1111-111111111111")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("rate rejection: code=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
		}
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("rate-limited bodies differ: %q vs %q", bodies[0], bodies[1])
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != "rate_limited" || payload.Error.Message != "rate limited" || !payload.Error.Retryable ||
		payload.Error.RequestID != "11111111-1111-1111-1111-111111111111" || strings.Contains(bodies[0], "known") ||
		strings.Contains(bodies[0], "unknown") || calls != 1 {
		t.Fatalf("rate-limited envelope=%+v body=%s calls=%d", payload.Error, bodies[0], calls)
	}
}

func TestConfigurationRejectsUnsafeRatesAndEarlyEviction(t *testing.T) {
	for _, rate := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		config := testConfig()
		config.IPRate = rate
		if _, err := New(config, nil); err == nil {
			t.Fatal("invalid rate accepted")
		}
	}
	for _, alter := range []func(*Config){
		func(c *Config) { c.InactiveTTL = time.Second },
		func(c *Config) { c.MaxEntries = 0 },
		func(c *Config) { c.IdentityBurst = 0 },
	} {
		config := testConfig()
		alter(&config)
		if _, err := New(config, nil); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
