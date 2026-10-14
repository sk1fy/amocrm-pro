package distribution

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type LeadPermission struct {
	Allowed bool
	Err     error
	Lead    *amocrm.DistributionLead
}
type permissionValue[T any] struct {
	once  sync.Once
	value T
	err   error
}

// This cache belongs to one HTTP read request and one installation. Assignment
// pre/post checks still use CanViewLead directly and cannot reuse this snapshot.
type permissionCRM struct {
	CRM
	installation uuid.UUID
	mu           sync.Mutex
	users        map[int64]*permissionValue[amocrm.DistributionUser]
	roles        map[int64]*permissionValue[amocrm.DistributionRights]
	leads        map[int64]amocrm.DistributionLead
	leadReads    map[int64]*permissionValue[amocrm.DistributionLead]
	batched      bool
}

func (c *permissionCRM) DistributionUser(ctx context.Context, install uuid.UUID, id int64) (amocrm.DistributionUser, error) {
	if install != c.installation {
		return amocrm.DistributionUser{}, ErrDenied
	}
	c.mu.Lock()
	v := c.users[id]
	if v == nil {
		v = &permissionValue[amocrm.DistributionUser]{}
		c.users[id] = v
	}
	c.mu.Unlock()
	v.once.Do(func() { v.value, v.err = c.CRM.DistributionUser(ctx, install, id) })
	return v.value, v.err
}
func (c *permissionCRM) DistributionRole(ctx context.Context, install uuid.UUID, id int64) (amocrm.DistributionRights, error) {
	if install != c.installation {
		return amocrm.DistributionRights{}, ErrDenied
	}
	c.mu.Lock()
	v := c.roles[id]
	if v == nil {
		v = &permissionValue[amocrm.DistributionRights]{}
		c.roles[id] = v
	}
	c.mu.Unlock()
	v.once.Do(func() { v.value, v.err = c.CRM.DistributionRole(ctx, install, id) })
	return v.value, v.err
}
func (c *permissionCRM) DistributionLead(ctx context.Context, install uuid.UUID, id int64) (amocrm.DistributionLead, error) {
	if install != c.installation {
		return amocrm.DistributionLead{}, ErrDenied
	}
	if c.batched {
		if lead, ok := c.leads[id]; ok {
			return lead, nil
		}
		return amocrm.DistributionLead{}, ErrUnavailable
	}
	c.mu.Lock()
	v := c.leadReads[id]
	if v == nil {
		v = &permissionValue[amocrm.DistributionLead]{}
		c.leadReads[id] = v
	}
	c.mu.Unlock()
	v.once.Do(func() { v.value, v.err = c.CRM.DistributionLead(ctx, install, id) })
	return v.value, v.err
}

// CanViewLeads preserves the single-lead policy while reducing actor/role reads
// and, on the production CRM client, reading lead snapshots in one exact-ID batch.
func CanViewLeads(ctx context.Context, crm CRM, install uuid.UUID, actor int64, ids []int64) map[int64]LeadPermission {
	out := make(map[int64]LeadPermission, len(ids))
	if len(ids) == 0 || len(ids) > 100 {
		return out
	}
	unique := make([]int64, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 {
			return out
		}
		if !seen[id] {
			unique = append(unique, id)
			seen[id] = true
		}
	}
	cache := &permissionCRM{CRM: crm, installation: install, users: map[int64]*permissionValue[amocrm.DistributionUser]{}, roles: map[int64]*permissionValue[amocrm.DistributionRights]{}, leadReads: map[int64]*permissionValue[amocrm.DistributionLead]{}}
	if batch, ok := crm.(interface {
		DistributionLeads(context.Context, uuid.UUID, []int64) (map[int64]amocrm.DistributionLead, error)
	}); ok {
		// Fetch independent sources together, avoiding two sequential network budgets.
		var err error
		var wait sync.WaitGroup
		wait.Add(1)
		go func() { defer wait.Done(); _, _ = cache.DistributionUser(ctx, install, actor) }()
		cache.leads, err = batch.DistributionLeads(ctx, install, unique)
		wait.Wait()
		if err != nil {
			for _, id := range unique {
				out[id] = LeadPermission{Err: ErrUnavailable}
			}
			return out
		}
		cache.batched = true
	}
	results := make([]LeadPermission, len(unique))
	jobs := make(chan int, len(unique))
	for i := range unique {
		jobs <- i
	}
	close(jobs)
	var wait sync.WaitGroup
	for i := 0; i < 4 && i < len(unique); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := range jobs {
				if err := ctx.Err(); err != nil {
					results[j].Err = err
					continue
				}
				allowed, err := CanViewLead(ctx, cache, install, actor, unique[j])
				results[j] = LeadPermission{Allowed: allowed && err == nil, Err: err}
				if allowed && err == nil {
					lead, e := cache.DistributionLead(ctx, install, unique[j])
					if e != nil {
						results[j] = LeadPermission{Err: e}
					} else {
						results[j].Lead = &lead
					}
				}
			}
		}()
	}
	wait.Wait()
	for i, id := range unique {
		out[id] = results[i]
	}
	return out
}
