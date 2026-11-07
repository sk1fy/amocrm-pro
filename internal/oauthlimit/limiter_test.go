package oauthlimit

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestWidgetInstallCallbackUsesSourceAndCodeBucket(t *testing.T) {
	install := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?code=x&client_id=client-a&from_widget=1", nil)
	if got := requestIdentity(install); got != "i\x00"+remoteIP(install)+"\x00x" {
		t.Fatalf("install identity = %q", got)
	}
	// A stateless callback without from_widget keeps the state bucket and still
	// fails state validation in the handler.
	plain := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?code=x&client_id=client-a", nil)
	if got := requestIdentity(plain); got != "s\x00" {
		t.Fatalf("plain identity = %q", got)
	}
	stated := httptest.NewRequest(http.MethodGet, "/oauth/amocrm/callback?state=s1&code=x&client_id=client-a&from_widget=1", nil)
	if got := requestIdentity(stated); got != "s\x00s1" {
		t.Fatalf("stated identity = %q", got)
	}
}

func TestRejectedSourceCannotFillIdentityCache(t *testing.T) {
	now := time.Now()
	config := testConfig()
	config.IPBurst = 1
	config.MaxEntries = 3
	l, err := newLimiter(config, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.allow("198.51.100.1", "first"); !ok {
		t.Fatal("initial admission")
	}
	for _, identity := range []string{"a", "b", "c", "d"} {
		if ok, _ := l.allow("198.51.100.1", identity); ok {
			t.Fatal("source budget bypassed")
		}
	}
	if ok, _ := l.allow("198.51.100.2", "unrelated"); !ok {
		t.Fatalf("rejected source filled identity cache: identities=%d", len(l.identities))
	}
}

func TestClientIPTrustsOnlyStrictRealIPFromConfiguredPeer(t *testing.T) {
	for _, tt := range []struct {
		name, peer string
		headers    []string
		trust      bool
		want       string
	}{
		{"no trust", "192.0.2.1:9", []string{"198.51.100.1"}, false, "192.0.2.1"},
		{"untrusted", "203.0.113.1:9", []string{"198.51.100.1"}, true, "203.0.113.1"},
		{"trusted IPv4", "192.0.2.1:9", []string{"198.51.100.1"}, true, "198.51.100.1"},
		{"trusted IPv6", "[2001:db8::1]:9", []string{"2001:db8:1::1"}, true, "2001:db8:1::1"},
		{"mapped", "[::ffff:192.0.2.1]:9", []string{"::ffff:198.51.100.1"}, true, "198.51.100.1"},
		{"multiple", "192.0.2.1:9", []string{"198.51.100.1", "198.51.100.2"}, true, "192.0.2.1"},
		{"comma", "192.0.2.1:9", []string{"198.51.100.1, 198.51.100.2"}, true, "192.0.2.1"},
		{"whitespace", "192.0.2.1:9", []string{" 198.51.100.1"}, true, "192.0.2.1"},
		{"port", "192.0.2.1:9", []string{"198.51.100.1:123"}, true, "192.0.2.1"},
		{"zone", "192.0.2.1:9", []string{"fe80::1%eth0"}, true, "192.0.2.1"},
		{"XFF ignored", "192.0.2.1:9", nil, true, "192.0.2.1"},
		{"invalid peer", "not-an-ip:9", []string{"198.51.100.1"}, true, "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := testConfig()
			if tt.trust {
				config.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/64")}
			}
			l, err := New(config, nil)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "/oauth/amocrm/callback", nil)
			r.RemoteAddr = tt.peer
			r.Header["X-Real-Ip"] = tt.headers
			r.Header.Set("X-Forwarded-For", "198.51.100.99")
			if got := l.clientIP(r); got != tt.want {
				t.Fatalf("source = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWidgetInstallCodesAndSourcesAreIndependent(t *testing.T) {
	config := testConfig()
	l, err := New(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for i, tt := range []struct {
		peer, code string
		want       int
	}{
		{"198.51.100.1:9", "first", 204}, {"198.51.100.1:9", "first", 429},
		{"198.51.100.1:9", "second", 204}, {"198.51.100.2:9", "first", 204},
	} {
		r := httptest.NewRequest("GET", "/oauth/amocrm/callback?from_widget=1&client_id=same&code="+tt.code, nil)
		r.RemoteAddr = tt.peer
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Fatalf("request %d = %d, want %d", i, w.Code, tt.want)
		}
	}
}
