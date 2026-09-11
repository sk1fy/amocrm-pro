package activitybridge

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	viewerGlobalRate  = 100
	viewerGlobalBurst = 200
	viewerMaxKeys     = 4096
	viewerInactiveTTL = 5 * time.Minute
)

type viewBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type viewKeyLimiter struct {
	mu          sync.Mutex
	buckets     map[[32]byte]*viewBucket
	global      *rate.Limiter
	now         func() time.Time
	nextCleanup time.Time
}

func newViewKeyLimiter() *viewKeyLimiter {
	return &viewKeyLimiter{buckets: make(map[[32]byte]*viewBucket), global: rate.NewLimiter(viewerGlobalRate, viewerGlobalBurst), now: time.Now}
}

func (l *viewKeyLimiter) Allow(key [32]byte) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// Shared admission precedes allocation and ResolveShare: changing an
	// unverified Bearer value must not buy another unrestricted request budget.
	if !l.global.AllowN(now, 1) {
		return false
	}
	if !now.Before(l.nextCleanup) {
		for key, entry := range l.buckets {
			if now.Sub(entry.lastSeen) >= viewerInactiveTTL {
				delete(l.buckets, key)
			}
		}
		l.nextCleanup = now.Add(time.Minute)
	}
	entry, ok := l.buckets[key]
	if !ok {
		// Do not evict active entries, which would reset their burst limits.
		if len(l.buckets) >= viewerMaxKeys {
			return false
		}
		entry = &viewBucket{limiter: rate.NewLimiter(20, 40)}
		l.buckets[key] = entry
	}
	entry.lastSeen = now
	return entry.limiter.AllowN(now, 1)
}
