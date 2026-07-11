package webhook

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

type IngressLimiterConfig struct {
	GlobalRate        rate.Limit
	GlobalBurst       int
	InstallationRate  rate.Limit
	InstallationBurst int
	InactiveTTL       time.Duration
}

type installationLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type IngressLimiter struct {
	global            *rate.Limiter
	installationRate  rate.Limit
	installationBurst int
	inactiveTTL       time.Duration
	sweepInterval     time.Duration
	metrics           *IngressMetrics
	now               func() time.Time

	mu            sync.Mutex
	installations map[uuid.UUID]*installationLimiter
}

func NewIngressLimiter(config IngressLimiterConfig, metrics *IngressMetrics) (*IngressLimiter, error) {
	return newIngressLimiter(config, metrics, time.Now)
}

func newIngressLimiter(
	config IngressLimiterConfig,
	metrics *IngressMetrics,
	now func() time.Time,
) (*IngressLimiter, error) {
	if !validRate(config.GlobalRate) || !validRate(config.InstallationRate) {
		return nil, errors.New("webhook ingress limiter rates must be finite and positive")
	}
	if config.GlobalBurst < 1 || config.InstallationBurst < 1 {
		return nil, errors.New("webhook ingress limiter bursts must be positive")
	}
	if config.InactiveTTL < time.Second {
		return nil, errors.New("webhook ingress limiter inactive TTL must be at least 1s")
	}
	if now == nil {
		return nil, errors.New("webhook ingress limiter clock is nil")
	}
	sweepInterval := config.InactiveTTL / 2
	if sweepInterval > time.Minute {
		sweepInterval = time.Minute
	}
	return &IngressLimiter{
		global:            rate.NewLimiter(config.GlobalRate, config.GlobalBurst),
		installationRate:  config.InstallationRate,
		installationBurst: config.InstallationBurst,
		inactiveTTL:       config.InactiveTTL,
		sweepInterval:     sweepInterval,
		metrics:           metrics,
		now:               now,
		installations:     make(map[uuid.UUID]*installationLimiter),
	}, nil
}

func validRate(value rate.Limit) bool {
	raw := float64(value)
	return raw > 0 && !math.IsNaN(raw) && !math.IsInf(raw, 0)
}

func (l *IngressLimiter) AllowGlobal() bool {
	allowed := l.global.AllowN(l.now(), 1)
	l.metrics.observeDecision(ingressScopeGlobal, allowed)
	return allowed
}

func (l *IngressLimiter) AllowInstallation(installationID uuid.UUID) bool {
	l.mu.Lock()
	now := l.now()
	entry, ok := l.installations[installationID]
	if !ok {
		entry = &installationLimiter{
			limiter: rate.NewLimiter(l.installationRate, l.installationBurst),
		}
		l.installations[installationID] = entry
		l.metrics.addEntry()
	}
	if entry.lastSeen.After(now) {
		now = entry.lastSeen
	}
	entry.lastSeen = now
	allowed := entry.limiter.AllowN(now, 1)
	l.mu.Unlock()

	l.metrics.observeDecision(ingressScopeInstallation, allowed)
	return allowed
}

// Run evicts inactive installation buckets outside the request path until the
// owning API lifecycle is canceled.
func (l *IngressLimiter) Run(ctx context.Context) {
	ticker := time.NewTicker(l.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			l.evictIdle(now)
		}
	}
}

func (l *IngressLimiter) evictIdle(now time.Time) int {
	cutoff := now.Add(-l.inactiveTTL)
	l.mu.Lock()
	evicted := 0
	for installationID, entry := range l.installations {
		if entry.lastSeen.Before(cutoff) {
			delete(l.installations, installationID)
			evicted++
		}
	}
	l.mu.Unlock()
	l.metrics.evictEntries(evicted)
	return evicted
}

func (l *IngressLimiter) installationCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.installations)
}
