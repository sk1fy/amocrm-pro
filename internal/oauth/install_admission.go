package oauth

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

const (
	installIntegrationCapacity = 8
	installProcessCapacity     = 64
	installRateWait            = 500 * time.Millisecond
)

var errInstallCapacity = errors.New("installation authorization capacity exhausted")

type InstallLimits struct {
	Rate        float64
	Burst       int
	InactiveTTL time.Duration
	MaxEntries  int
}

type installEntry struct {
	limiter *rate.Limiter
	active  int
	lastUse time.Time
}

type installAdmission struct {
	mu      sync.Mutex
	limits  InstallLimits
	entries map[uuid.UUID]*installEntry
	active  int
}

type installPermit struct {
	gate  *installAdmission
	entry *installEntry
}

func newInstallAdmission(limits InstallLimits) (*installAdmission, error) {
	if limits.Rate <= 0 || math.IsNaN(limits.Rate) || math.IsInf(limits.Rate, 0) || limits.Burst < 1 || limits.MaxEntries < 1 || limits.InactiveTTL < time.Second || limits.InactiveTTL.Seconds() < float64(limits.Burst)/limits.Rate {
		return nil, errors.New("invalid installation OAuth limits")
	}
	return &installAdmission{limits: limits, entries: make(map[uuid.UUID]*installEntry)}, nil
}

func (g *installAdmission) acquire(id uuid.UUID) (*installPermit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.active >= installProcessCapacity {
		return nil, errInstallCapacity
	}
	entry := g.entries[id]
	if entry == nil {
		if len(g.entries) >= g.limits.MaxEntries {
			for key, candidate := range g.entries {
				if candidate.active == 0 && now.Sub(candidate.lastUse) >= g.limits.InactiveTTL && candidate.limiter.TokensAt(now) >= float64(g.limits.Burst) {
					delete(g.entries, key)
				}
			}
			if len(g.entries) >= g.limits.MaxEntries {
				return nil, errInstallCapacity
			}
		}
		entry = &installEntry{limiter: rate.NewLimiter(rate.Limit(g.limits.Rate), g.limits.Burst)}
		g.entries[id] = entry
	}
	if entry.active >= installIntegrationCapacity {
		return nil, errInstallCapacity
	}
	entry.active++
	entry.lastUse = now
	g.active++
	return &installPermit{gate: g, entry: entry}, nil
}

func (p *installPermit) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, installRateWait)
	defer cancel()
	err := p.entry.limiter.Wait(waitCtx)
	if parentErr := ctx.Err(); parentErr != nil {
		return parentErr
	}
	if err != nil || waitCtx.Err() != nil {
		return errInstallCapacity
	}
	return nil
}

func (p *installPermit) release() {
	p.gate.mu.Lock()
	defer p.gate.mu.Unlock()
	p.entry.active--
	p.entry.lastUse = time.Now()
	p.gate.active--
}
