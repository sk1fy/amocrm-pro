package activitybridge

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestViewerLimiterBoundsDistinctCredentialsAndReclaimsIdleKeys(t *testing.T) {
	l := newViewKeyLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	accepted := 0
	for i := 0; i < 10000; i++ {
		var key [32]byte
		binary.LittleEndian.PutUint64(key[:], uint64(i))
		if l.Allow(key) {
			accepted++
		}
	}
	if accepted != viewerGlobalBurst || len(l.buckets) != viewerGlobalBurst {
		t.Fatalf("rotating credentials: accepted=%d entries=%d", accepted, len(l.buckets))
	}
	now = now.Add(viewerInactiveTTL + time.Minute)
	if !l.Allow(sha256.Sum256([]byte("fresh"))) || len(l.buckets) != 1 {
		t.Fatalf("idle entries not reclaimed: %d", len(l.buckets))
	}
}

func TestViewerLimiterHardCapDoesNotResetActiveBudgets(t *testing.T) {
	l := newViewKeyLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < viewerMaxKeys; i++ {
		// 50 distinct requests per second keeps all entries inside the TTL.
		now = now.Add(20 * time.Millisecond)
		key := sha256.Sum256([]byte(fmt.Sprint(i)))
		if !l.Allow(key) {
			t.Fatalf("entry %d unexpectedly denied", i)
		}
	}
	now = now.Add(time.Second)
	if l.Allow(sha256.Sum256([]byte("overflow"))) || len(l.buckets) != viewerMaxKeys {
		t.Fatal("exceeded hard cap")
	}
	if !l.Allow(sha256.Sum256([]byte("0"))) {
		t.Fatal("active existing key denied at map capacity")
	}
}

func TestViewerHTTPBudgetRejectsUnknownKeysBeforeLookup(t *testing.T) {
	b := New(nil, nil, nil, nil)
	b.ConfigureShare([]string{"https://activity.example.invalid"}, "")
	now := time.Now()
	b.viewerLimiter.now = func() time.Time { return now }
	calls := 0
	h := b.viewerAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(404) }))
	for i := 0; i < viewerGlobalBurst+1; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/v1/activity/view/panel", nil)
		r.Header.Set("Authorization", fmt.Sprintf("Bearer unknown-%d", i))
		h.ServeHTTP(w, r)
		if i == viewerGlobalBurst && (w.Code != 429 || w.Header().Get("Retry-After") != "1") {
			t.Fatalf("no bounded HTTP admission: %d", w.Code)
		}
	}
	if calls != viewerGlobalBurst {
		t.Fatalf("excess request reached downstream: %d", calls)
	}
}
