package webhook

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

func TestIngressLimiterValidatesConfiguration(t *testing.T) {
	valid := IngressLimiterConfig{
		GlobalRate: 1, GlobalBurst: 1,
		InstallationRate: 1, InstallationBurst: 1,
		InactiveTTL: time.Minute,
	}
	for _, mutate := range []func(*IngressLimiterConfig){
		func(config *IngressLimiterConfig) { config.GlobalRate = 0 },
		func(config *IngressLimiterConfig) { config.InstallationRate = rate.Limit(math.NaN()) },
		func(config *IngressLimiterConfig) { config.GlobalBurst = 0 },
		func(config *IngressLimiterConfig) { config.InstallationBurst = 0 },
		func(config *IngressLimiterConfig) { config.InactiveTTL = 0 },
	} {
		config := valid
		mutate(&config)
		if _, err := NewIngressLimiter(config, nil); err == nil {
			t.Fatalf("expected invalid config rejection: %+v", config)
		}
	}
}

func TestIngressLimiterEvictsOnlyIdleInstallationsAndRecreatesBucket(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	registry := prometheus.NewRegistry()
	metrics := NewIngressMetrics(registry)
	limiter, err := newIngressLimiter(IngressLimiterConfig{
		GlobalRate: 100, GlobalBurst: 100,
		InstallationRate: 1, InstallationBurst: 1,
		InactiveTTL: time.Minute,
	}, metrics, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, active := uuid.New(), uuid.New()
	if !limiter.AllowInstallation(first) || !limiter.AllowInstallation(active) {
		t.Fatal("initial installation requests must be allowed")
	}
	assertIngressMetricValue(t, registry, "amocrm_webhook_ingress_limiter_entries", 2)
	now = now.Add(30 * time.Second)
	_ = limiter.AllowInstallation(active)
	now = now.Add(31 * time.Second)
	if evicted := limiter.evictIdle(now); evicted != 1 {
		t.Fatalf("evicted entries = %d, want 1", evicted)
	}
	if count := limiter.installationCount(); count != 1 {
		t.Fatalf("installation entries = %d, want 1", count)
	}
	assertIngressMetricValue(t, registry, "amocrm_webhook_ingress_limiter_entries", 1)
	assertIngressMetricValue(t, registry, "amocrm_webhook_ingress_limiter_evictions_total", 1)
	if !limiter.AllowInstallation(first) {
		t.Fatal("request after idle eviction must use a fresh bucket")
	}
	if count := limiter.installationCount(); count != 2 {
		t.Fatalf("installation entries after recreation = %d, want 2", count)
	}
	assertIngressMetricValue(t, registry, "amocrm_webhook_ingress_limiter_entries", 2)
}

func TestIngressLimiterRefreshAndActualEvictionRaceUsesOneBucket(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter, err := newIngressLimiter(IngressLimiterConfig{
		GlobalRate: 10_000, GlobalBurst: 10_000,
		InstallationRate: 1, InstallationBurst: 1,
		InactiveTTL: time.Minute,
	}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	installationID := uuid.New()
	if !limiter.AllowInstallation(installationID) {
		t.Fatal("initial request must be allowed")
	}
	now = now.Add(2 * time.Minute)
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_ = limiter.AllowInstallation(installationID)
	}()
	go func() {
		defer workers.Done()
		<-start
		_ = limiter.evictIdle(now)
	}()
	close(start)
	workers.Wait()
	if count := limiter.installationCount(); count != 1 {
		t.Fatalf("installation entries = %d, want one shared bucket", count)
	}
	if limiter.AllowInstallation(installationID) {
		t.Fatal("same-timestamp request must not regain a second burst token")
	}
}

func TestIngressLimiterLastSeenNeverMovesBackward(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	times := []time.Time{base.Add(2 * time.Minute), base}
	limiter, err := newIngressLimiter(IngressLimiterConfig{
		GlobalRate: 10, GlobalBurst: 10,
		InstallationRate: 1, InstallationBurst: 1,
		InactiveTTL: time.Minute,
	}, nil, func() time.Time {
		current := times[0]
		times = times[1:]
		return current
	})
	if err != nil {
		t.Fatal(err)
	}
	installationID := uuid.New()
	_ = limiter.AllowInstallation(installationID)
	_ = limiter.AllowInstallation(installationID)
	if evicted := limiter.evictIdle(base.Add(150 * time.Second)); evicted != 0 {
		t.Fatalf("recent entry was evicted after an older clock observation: %d", evicted)
	}
}

func assertIngressMetricValue(
	t *testing.T,
	registry *prometheus.Registry,
	name string,
	expected float64,
) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name || len(family.Metric) != 1 {
			continue
		}
		metric := family.Metric[0]
		value := metric.GetGauge().GetValue()
		if metric.Counter != nil {
			value = metric.GetCounter().GetValue()
		}
		if value != expected {
			t.Fatalf("metric %s = %v, want %v", name, value, expected)
		}
		return
	}
	t.Fatalf("metric %s not found", name)
}
