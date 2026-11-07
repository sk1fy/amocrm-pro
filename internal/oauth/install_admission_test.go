package oauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInstallAdmissionCapacityAndRelease(t *testing.T) {
	g, err := newInstallAdmission(InstallLimits{Rate: 2, Burst: 8, InactiveTTL: time.Minute, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	var permits []*installPermit
	for i := range 8 {
		id := uuid.New()
		for range 8 {
			p, err := g.acquire(id)
			if err != nil {
				t.Fatalf("integration %d: %v", i, err)
			}
			permits = append(permits, p)
		}
		if _, err := g.acquire(id); !errors.Is(err, errInstallCapacity) {
			t.Fatalf("per-integration capacity: %v", err)
		}
	}
	if _, err := g.acquire(uuid.New()); !errors.Is(err, errInstallCapacity) {
		t.Fatalf("process capacity: %v", err)
	}
	for _, p := range permits {
		p.release()
	}
	p, err := g.acquire(uuid.New())
	if err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
	p.release()
}

func TestInstallAdmissionEvictsOnlyInactiveRefilledEntries(t *testing.T) {
	g, err := newInstallAdmission(InstallLimits{Rate: 1, Burst: 1, InactiveTTL: time.Second, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	p, err := g.acquire(id)
	if err != nil {
		t.Fatal(err)
	}
	p.entry.lastUse = time.Now().Add(-time.Hour)
	if _, err := g.acquire(uuid.New()); !errors.Is(err, errInstallCapacity) {
		t.Fatalf("evicted active permit: %v", err)
	}
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.release()
	p.entry.lastUse = time.Now().Add(-time.Hour)
	if _, err := g.acquire(uuid.New()); !errors.Is(err, errInstallCapacity) {
		t.Fatalf("reset depleted burst: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	p, err = g.acquire(uuid.New())
	if err != nil {
		t.Fatalf("refilled idle entry not evicted: %v", err)
	}
	p.release()
}

func TestInstallWaitCancellationRequiresRestart(t *testing.T) {
	g, err := newInstallAdmission(InstallLimits{Rate: 1, Burst: 1, InactiveTTL: time.Minute, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.acquire(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.wait(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("cancelled claimed wait=%v", err)
	}
}
