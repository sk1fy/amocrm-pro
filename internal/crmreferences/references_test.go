package crmreferences

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readerFunc func(context.Context, uuid.UUID, int64) (Data, error)

func (f readerFunc) CRMReferences(ctx context.Context, i uuid.UUID, a int64) (Data, error) {
	return f(ctx, i, a)
}
func data() Data {
	return Data{Users: []User{{ID: "11", Name: "<User>\u2028\\u2028", IsActive: true}, {ID: "2", Name: "Second"}}, Groups: []Group{{ID: "7", Name: "Empty group"}}, Pipelines: []Pipeline{{ID: "8", Name: "Покупатели", Statuses: []LeadStatus{{Status: Status{ID: "143"}, Kind: "lost"}, {Status: Status{ID: "10"}, Kind: "open"}, {Status: Status{ID: "142"}, Kind: "won"}}}}, Customers: Customers{Availability: "unsupported", Statuses: []Status{}}}
}
func scope() Scope {
	return Scope{CompanyID: uuid.NewString(), InstallationID: uuid.NewString(), IntegrationID: uuid.NewString(), AccountID: "123", Generation: 1, Product: "task-control"}
}
func TestCanonicalRevisionAndValidation(t *testing.T) {
	d := data()
	s := Snapshot{Scope: scope(), Users: d.Users, Groups: d.Groups, Pipelines: d.Pipelines, Customers: d.Customers}
	if err := Finalize(&s); err != nil {
		t.Fatal(err)
	}
	revision := s.Revision
	if s.Users[0].ID != "2" || s.Pipelines[0].Statuses[0].ID != "10" {
		t.Fatal("arrays not sorted numerically")
	}
	s.Users[0], s.Users[1] = s.Users[1], s.Users[0]
	s.FreshUntil = time.Now()
	s.Customers.CheckedAt = time.Now()
	code := "forbidden"
	s.Customers.ErrorCode = &code
	if err := Finalize(&s); err != nil || s.Revision != revision {
		t.Fatal("transport/time/order changed semantic revision", err)
	}
	s.Customers.Availability = "unknown"
	_ = Finalize(&s)
	if s.Revision == revision {
		t.Fatal("availability omitted")
	}
	s.Users[0].ID = "01"
	if !errors.Is(Finalize(&s), ErrIncomplete) {
		t.Fatal("noncanonical ID accepted")
	}
	s.Users[0].ID = "9007199254740992"
	if !errors.Is(Finalize(&s), ErrIncomplete) {
		t.Fatal("unsafe ID accepted")
	}
	s.Users = []User{{ID: "1", GroupID: &code}}
	if !errors.Is(Finalize(&s), ErrIncomplete) {
		t.Fatal("foreign group accepted")
	}
}
func TestCanonicalJSONKnownJCSStrings(t *testing.T) {
	got, err := canonicalJSON(map[string]any{"z": "<>&\u2028\u2029\\u2028", "a": nil})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"a\":null,\"z\":\"<>&\u2028\u2029\\\\u2028\"}"
	if string(got) != want {
		t.Fatalf("%q != %q", got, want)
	}
}
func TestReferenceLimitsNeverTruncate(t *testing.T) {
	d := data()
	s := Snapshot{Users: d.Users, Groups: d.Groups, Pipelines: d.Pipelines, Customers: d.Customers}
	s.Users = make([]User, 1001)
	if !errors.Is(Finalize(&s), ErrTooLarge) {
		t.Fatal("entity overflow accepted")
	}
	s.Users = []User{{ID: "1", Name: strings.Repeat("x", MaxBytes)}}
	if !errors.Is(Finalize(&s), ErrTooLarge) {
		t.Fatal("payload overflow accepted")
	}
	s.Users = []User{}
	s.Pipelines[0].Statuses[0].Kind = "mystery"
	if !errors.Is(Finalize(&s), ErrIncomplete) {
		t.Fatal("unknown stage kind accepted")
	}
}
func TestProviderSingleflightRefreshTTLAndMutation(t *testing.T) {
	now := time.Now().UTC()
	p := &Provider{Now: func() time.Time { return now }}
	s := scope()
	var calls atomic.Int32
	reader := readerFunc(func(context.Context, uuid.UUID, int64) (Data, error) { calls.Add(1); return data(), nil })
	fence := func(context.Context) error { return nil }
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Get(context.Background(), "scope", s, reader, fence); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate CRM fetches", calls.Load())
	}
	a, err := p.Get(context.Background(), "scope", s, reader, fence)
	if err != nil {
		t.Fatal(err)
	}
	a.Users[0].Name = "mutated"
	now = now.Add(119 * time.Second)
	b, _ := p.Get(context.Background(), "scope", s, reader, fence)
	if calls.Load() != 1 || b.Users[0].Name == "mutated" || !a.FreshUntil.Equal(b.FreshUntil) {
		t.Fatal("cache hit altered data or lease")
	}
	now = now.Add(time.Second)
	c, _ := p.Get(context.Background(), "scope", s, reader, fence)
	if calls.Load() != 2 || a.Revision != c.Revision || !c.FreshUntil.Equal(now.Add(5*time.Minute)) {
		t.Fatal("120s refresh policy", calls.Load())
	}
	denied := errors.New("revoked")
	if _, err = p.Get(context.Background(), "scope", s, reader, func(context.Context) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("cache granted access")
	}
	if len(p.cache) != 0 {
		t.Fatal("revoked cache retained")
	}
}
func TestProviderLateRevocationAndFetchStartTTL(t *testing.T) {
	now := time.Now().UTC()
	p := &Provider{Now: func() time.Time { return now }}
	revoked := false
	denied := errors.New("revoked")
	reader := readerFunc(func(context.Context, uuid.UUID, int64) (Data, error) { revoked = true; return data(), nil })
	if _, err := p.Get(context.Background(), "key", scope(), reader, func(context.Context) error {
		if revoked {
			return denied
		}
		return nil
	}); !errors.Is(err, denied) {
		t.Fatal("late revoked result accepted", err)
	}
	if len(p.cache) != 0 {
		t.Fatal("late fetch resurrected cache")
	}
	reader = readerFunc(func(context.Context, uuid.UUID, int64) (Data, error) {
		now = now.Add(6 * time.Minute)
		return data(), nil
	})
	if _, err := p.Get(context.Background(), "key", scope(), reader, func(context.Context) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired fetch accepted", err)
	}
}
func TestProviderBoundedScopesAndCancelledWaiter(t *testing.T) {
	p := &Provider{}
	fence := func(context.Context) error { return nil }
	reader := readerFunc(func(context.Context, uuid.UUID, int64) (Data, error) { return data(), nil })
	for i := 0; i < 270; i++ {
		s := scope()
		if _, err := p.Get(context.Background(), s.InstallationID, s, reader, fence); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.cache) != 256 {
		t.Fatal("unbounded cache", len(p.cache))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Get(ctx, "cancelled", scope(), readerFunc(func(ctx context.Context, _ uuid.UUID, _ int64) (Data, error) { <-ctx.Done(); return Data{}, ctx.Err() }), fence)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}

func TestReferenceSnapshotSharedWireFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference-snapshot-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	revision := s.Revision
	if err = Finalize(&s); err != nil {
		t.Fatal(err)
	}
	if revision != s.Revision {
		t.Fatalf("shared JCS hash mismatch: fixture=%s calculated=%s", revision, s.Revision)
	}
	if s.Provenance.FetchStartedAt.Format(time.RFC3339) != "2026-10-16T10:00:00Z" || s.Customers.Availability != "supported" || len(s.Users) != 3 || s.Users[0].ID != "2" || s.Users[2].ID != "9007199254740991" {
		t.Fatal("shared fixture lost semantics")
	}
}
