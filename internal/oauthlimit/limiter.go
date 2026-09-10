// Package oauthlimit bounds unauthenticated OAuth start/callback traffic per API
// process. Buckets are keyed by hashed client IP and hashed integration_code or
// state so admission cannot reveal whether an integration exists.
package oauthlimit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxHashedCode  = 128
	maxHashedState = 256
)

type Config struct {
	IPRate        float64
	IPBurst       int
	IdentityRate  float64
	IdentityBurst int
	InactiveTTL   time.Duration
	MaxEntries    int
}

type bucket struct {
	tokens   float64
	updated  time.Time
	lastSeen time.Time
}

type Limiter struct {
	config     Config
	now        func() time.Time
	nextSweep  time.Time
	mu         sync.Mutex
	ips        map[[32]byte]*bucket
	identities map[[32]byte]*bucket
	decisions  *prometheus.CounterVec
	entries    *prometheus.GaugeVec
}

func New(config Config, registerer prometheus.Registerer) (*Limiter, error) {
	return newLimiter(config, registerer, time.Now)
}

func newLimiter(config Config, registerer prometheus.Registerer, now func() time.Time) (*Limiter, error) {
	for _, value := range []float64{config.IPRate, config.IdentityRate} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("oauth limiter rates must be finite and positive")
		}
	}
	if config.IPBurst < 1 || config.IdentityBurst < 1 || config.MaxEntries < 1 {
		return nil, errors.New("oauth limiter bursts and maximum entries must be positive")
	}
	refillSeconds := math.Max(float64(config.IPBurst)/config.IPRate, float64(config.IdentityBurst)/config.IdentityRate)
	if config.InactiveTTL < time.Second || config.InactiveTTL.Seconds() < refillSeconds {
		return nil, errors.New("oauth limiter inactive TTL must cover a full burst refill and be at least 1s")
	}
	if now == nil {
		return nil, errors.New("oauth limiter clock is required")
	}
	l := &Limiter{
		config: config, now: now,
		ips: make(map[[32]byte]*bucket), identities: make(map[[32]byte]*bucket),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "amocrm_oauth_limit_decisions_total",
			Help: "OAuth start/callback admission by bounded limiter scope and outcome.",
		}, []string{"scope", "outcome"}),
		entries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "amocrm_oauth_limit_entries",
			Help: "Current bounded OAuth token bucket cache entries.",
		}, []string{"scope"}),
	}
	if registerer != nil {
		registerer.MustRegister(l.decisions, l.entries)
	}
	return l, nil
}

// allow checks IP and identity budgets under one mutex and spends neither on
// rejection. Known and unknown integration codes therefore share the same 429
// contract once either bucket is exhausted.
func (l *Limiter) allow(ip, identity string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ipKey := hashKey("ip", ip)
	idKey := hashKey("id", identity)
	ipBucket, idBucket := l.ips[ipKey], l.identities[idKey]
	if (ipBucket == nil && len(l.ips) >= l.config.MaxEntries) ||
		(idBucket == nil && len(l.identities) >= l.config.MaxEntries) {
		l.evictLocked(now)
		ipBucket, idBucket = l.ips[ipKey], l.identities[idKey]
		if (ipBucket == nil && len(l.ips) >= l.config.MaxEntries) ||
			(idBucket == nil && len(l.identities) >= l.config.MaxEntries) {
			l.decisions.WithLabelValues("capacity", "rejected").Inc()
			return false, l.config.InactiveTTL
		}
	}
	if ipBucket == nil {
		ipBucket = &bucket{tokens: float64(l.config.IPBurst), updated: now, lastSeen: now}
		l.ips[ipKey] = ipBucket
	}
	if idBucket == nil {
		idBucket = &bucket{tokens: float64(l.config.IdentityBurst), updated: now, lastSeen: now}
		l.identities[idKey] = idBucket
	}
	l.updateEntries()
	refill(ipBucket, now, l.config.IPRate, l.config.IPBurst)
	refill(idBucket, now, l.config.IdentityRate, l.config.IdentityBurst)
	retry := 0.0
	for _, check := range []struct {
		scope  string
		bucket *bucket
		rate   float64
	}{
		{"ip", ipBucket, l.config.IPRate},
		{"identity", idBucket, l.config.IdentityRate},
	} {
		if check.bucket.tokens < 1 {
			retry = math.Max(retry, (1-check.bucket.tokens)/check.rate)
			l.decisions.WithLabelValues(check.scope, "rejected").Inc()
		}
	}
	if retry > 0 {
		return false, time.Duration(math.Ceil(retry * float64(time.Second)))
	}
	ipBucket.tokens--
	idBucket.tokens--
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

func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed, retry := l.allow(remoteIP(r), requestIdentity(r))
		if !allowed {
			w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64(math.Ceil(retry.Seconds()))), 10))
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(jsonError{Error: jsonErrorFields{
				Code: "rate_limited", Message: "rate limited",
				RequestID: w.Header().Get("X-Request-ID"), Retryable: true,
			}})
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
	for key, b := range l.ips {
		if b.lastSeen.Before(cutoff) {
			delete(l.ips, key)
		}
	}
	for key, b := range l.identities {
		if b.lastSeen.Before(cutoff) {
			delete(l.identities, key)
		}
	}
	l.updateEntries()
}

func (l *Limiter) updateEntries() {
	l.entries.WithLabelValues("ip").Set(float64(len(l.ips)))
	l.entries.WithLabelValues("identity").Set(float64(len(l.identities)))
}

func hashKey(kind, value string) [32]byte {
	return sha256.Sum256([]byte(kind + "\x00" + value))
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

func requestIdentity(r *http.Request) string {
	if strings.HasSuffix(r.URL.Path, "/callback") {
		return "s\x00" + clip(r.URL.Query().Get("state"), maxHashedState)
	}
	return "c\x00" + clip(r.URL.Query().Get("integration_code"), maxHashedCode)
}

func clip(value string, maxLen int) string {
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

type jsonError struct {
	Error jsonErrorFields `json:"error"`
}

type jsonErrorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}
