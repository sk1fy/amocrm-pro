// Package widgetlimit bounds authenticated widget traffic per API process.
// Buckets use verified identities and are shared across all widget endpoints.
package widgetlimit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type Config struct {
	IntegrationRate   float64
	IntegrationBurst  int
	InstallationRate  float64
	InstallationBurst int
	InactiveTTL       time.Duration
	MaxEntries        int
}

type bucket struct {
	tokens   float64
	updated  time.Time
	lastSeen time.Time
}

type installationKey struct{ integration, installation uuid.UUID }

type Limiter struct {
	config        Config
	now           func() time.Time
	nextSweep     time.Time
	mu            sync.Mutex
	integrations  map[uuid.UUID]*bucket
	installations map[installationKey]*bucket
	decisions     *prometheus.CounterVec
	entries       *prometheus.GaugeVec
}

func New(config Config, registerer prometheus.Registerer) (*Limiter, error) {
	return newLimiter(config, registerer, time.Now)
}

func newLimiter(config Config, registerer prometheus.Registerer, now func() time.Time) (*Limiter, error) {
	for _, value := range []float64{config.IntegrationRate, config.InstallationRate} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("widget limiter rates must be finite and positive")
		}
	}
	if config.IntegrationBurst < 1 || config.InstallationBurst < 1 || config.MaxEntries < 1 {
		return nil, errors.New("widget limiter bursts and maximum entries must be positive")
	}
	// Eviction must not refill a bucket faster than normal token replenishment.
	refillSeconds := math.Max(float64(config.IntegrationBurst)/config.IntegrationRate,
		float64(config.InstallationBurst)/config.InstallationRate)
	if config.InactiveTTL < time.Second || config.InactiveTTL.Seconds() < refillSeconds {
		return nil, errors.New("widget limiter inactive TTL must cover a full burst refill and be at least 1s")
	}
	if now == nil {
		return nil, errors.New("widget limiter clock is required")
	}
	l := &Limiter{
		config: config, now: now,
		integrations: make(map[uuid.UUID]*bucket), installations: make(map[installationKey]*bucket),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "amocrm_widget_limit_decisions_total", Help: "Widget request admission by bounded limiter scope and outcome.",
		}, []string{"scope", "outcome"}),
		entries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "amocrm_widget_limit_entries", Help: "Current bounded widget token bucket cache entries.",
		}, []string{"scope"}),
	}
	if registerer != nil {
		registerer.MustRegister(l.decisions, l.entries)
	}
	return l, nil
}

// allow checks both budgets under one mutex and spends neither on rejection.
// A busy installation therefore cannot drain its integration's remaining
// budget by repeatedly issuing requests already rejected by its own bucket.
func (l *Limiter) allow(integration, installation uuid.UUID) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := installationKey{integration, installation}
	i, a := l.integrations[integration], l.installations[key]
	if (i == nil && len(l.integrations) >= l.config.MaxEntries) ||
		(a == nil && len(l.installations) >= l.config.MaxEntries) {
		l.evictLocked(now)
		i, a = l.integrations[integration], l.installations[key]
		if (i == nil && len(l.integrations) >= l.config.MaxEntries) ||
			(a == nil && len(l.installations) >= l.config.MaxEntries) {
			l.decisions.WithLabelValues("capacity", "rejected").Inc()
			return false, l.config.InactiveTTL
		}
	}
	if i == nil {
		i = &bucket{tokens: float64(l.config.IntegrationBurst), updated: now, lastSeen: now}
		l.integrations[integration] = i
	}
	if a == nil {
		a = &bucket{tokens: float64(l.config.InstallationBurst), updated: now, lastSeen: now}
		l.installations[key] = a
	}
	l.updateEntries()
	refill(i, now, l.config.IntegrationRate, l.config.IntegrationBurst)
	refill(a, now, l.config.InstallationRate, l.config.InstallationBurst)
	retry := 0.0
	for _, check := range []struct {
		scope  string
		bucket *bucket
		rate   float64
	}{
		{"integration", i, l.config.IntegrationRate}, {"installation", a, l.config.InstallationRate},
	} {
		if check.bucket.tokens < 1 {
			retry = math.Max(retry, (1-check.bucket.tokens)/check.rate)
			l.decisions.WithLabelValues(check.scope, "rejected").Inc()
		}
	}
	if retry > 0 {
		return false, time.Duration(math.Ceil(retry * float64(time.Second)))
	}
	i.tokens--
	a.tokens--
	l.decisions.WithLabelValues("tenant", "allowed").Inc()
	return true, 0
}

func refill(b *bucket, now time.Time, rate float64, burst int) {
	if now.After(b.updated) {
		b.tokens = math.Min(float64(burst), b.tokens+now.Sub(b.updated).Seconds()*rate)
		b.updated = now
	}
	if now.After(b.lastSeen) {
		b.lastSeen = now
	}
}

// Middleware must run after JWT verification and origin/issuer binding, before
// token consumption or durable command admission. OPTIONS bypasses this layer
// in the CORS middleware and is never assigned an unverified tenant bucket.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := widgetauth.PrincipalFromContext(r.Context())
		if !ok || principal.IntegrationID == uuid.Nil || principal.InstallationID == uuid.Nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		allowed, retry := l.allow(principal.IntegrationID, principal.InstallationID)
		if !allowed {
			w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(math.Ceil(retry.Seconds()))), 10))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "rate_limited"}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) Run(ctx context.Context) {
	ticker := time.NewTicker(min(time.Minute, l.config.InactiveTTL/2))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			l.evictLocked(l.now())
			l.mu.Unlock()
		}
	}
}

func (l *Limiter) evictLocked(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(min(time.Minute, l.config.InactiveTTL/2))
	cutoff := now.Add(-l.config.InactiveTTL)
	for key, b := range l.integrations {
		if b.lastSeen.Before(cutoff) {
			delete(l.integrations, key)
		}
	}
	for key, b := range l.installations {
		if b.lastSeen.Before(cutoff) {
			delete(l.installations, key)
		}
	}
	l.updateEntries()
}

func (l *Limiter) updateEntries() {
	l.entries.WithLabelValues("integration").Set(float64(len(l.integrations)))
	l.entries.WithLabelValues("installation").Set(float64(len(l.installations)))
}
