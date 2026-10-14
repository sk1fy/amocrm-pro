package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"sync/atomic"
	"testing"
	"time"
)

type batchPolicyCRM struct {
	crmFake
	usersRead, rolesRead, leadsRead atomic.Int32
	delay                           time.Duration
	missing                         bool
	hook                            func()
	installation                    uuid.UUID
}

func (f *batchPolicyCRM) DistributionUser(ctx context.Context, install uuid.UUID, id int64) (amocrm.DistributionUser, error) {
	f.usersRead.Add(1)
	if install != f.installation {
		return amocrm.DistributionUser{}, errors.New("wrong installation")
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return amocrm.DistributionUser{}, ctx.Err()
	}
	return f.crmFake.DistributionUser(ctx, install, id)
}
func (f *batchPolicyCRM) DistributionRole(ctx context.Context, install uuid.UUID, id int64) (amocrm.DistributionRights, error) {
	f.rolesRead.Add(1)
	return f.crmFake.DistributionRole(ctx, install, id)
}
func (f *batchPolicyCRM) DistributionLeads(ctx context.Context, install uuid.UUID, ids []int64) (map[int64]amocrm.DistributionLead, error) {
	f.leadsRead.Add(1)
	if install != f.installation {
		return nil, errors.New("wrong installation")
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.hook != nil {
		f.hook()
	}
	result := map[int64]amocrm.DistributionLead{}
	for _, id := range ids {
		if f.missing && id == 11 {
			continue
		}
		result[id] = amocrm.DistributionLead{ID: id, PipelineID: 20, StatusID: 30, ResponsibleUserID: 2}
	}
	return result, nil
}
func TestPermissionBatchSlowSourcesReuseRequestOnlyAndPolicyRevocation(t *testing.T) {
	install := uuid.New()
	role := int64(9)
	f := &batchPolicyCRM{installation: install, delay: 50 * time.Millisecond, crmFake: crmFake{users: map[int64]amocrm.DistributionUser{1: {ID: 1, Rights: amocrm.DistributionRights{IsActive: ptr(true), RoleID: &role}}}, role: amocrm.DistributionRights{Leads: map[string]string{"view": "A"}}}}
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(10 + i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	results := CanViewLeads(ctx, f, install, 1, ids)
	for _, id := range ids {
		if !results[id].Allowed || results[id].Err != nil || results[id].Lead == nil {
			t.Fatalf("%d: %+v", id, results[id])
		}
	}
	if f.usersRead.Load() != 1 || f.rolesRead.Load() != 1 || f.leadsRead.Load() != 1 {
		t.Fatalf("source reads users=%d roles=%d leads=%d", f.usersRead.Load(), f.rolesRead.Load(), f.leadsRead.Load())
	}
	// A second HTTP request cannot inherit either actor rights or lead data.
	f.role.Leads["view"] = "D"
	results = CanViewLeads(context.Background(), f, install, 1, []int64{10, 10, 11})
	if results[10].Allowed || results[11].Allowed || len(results) != 2 || f.usersRead.Load() != 2 || f.rolesRead.Load() != 2 || f.leadsRead.Load() != 2 {
		t.Fatal("stale/duplicated policy", results)
	}
	results = CanViewLeads(context.Background(), f, uuid.New(), 1, []int64{10})
	if results[10].Allowed || results[10].Err == nil {
		t.Fatal("cross-installation source disclosed")
	}
}
func TestPermissionBatchMissingAndUnavailableSourcesFailClosed(t *testing.T) {
	install := uuid.New()
	f := &batchPolicyCRM{installation: install, missing: true, crmFake: crmFake{users: map[int64]amocrm.DistributionUser{1: {ID: 1, Rights: amocrm.DistributionRights{IsActive: ptr(true), IsAdmin: true}}}}}
	results := CanViewLeads(context.Background(), f, install, 1, []int64{10, 11})
	if !results[10].Allowed || results[11].Allowed || !errors.Is(results[11].Err, ErrUnavailable) {
		t.Fatal(results)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, result := range CanViewLeads(ctx, f, install, 1, []int64{10, 11}) {
		if result.Allowed || result.Err == nil {
			t.Fatal("cancelled source allowed", result)
		}
	}
}

type subscriberBatchCRM struct {
	*batchPolicyCRM
	subscriptionReads atomic.Int32
}

func (f *subscriberBatchCRM) DistributionSubscriptions(_ context.Context, _ uuid.UUID, id int64) ([]amocrm.Subscription, error) {
	f.subscriptionReads.Add(1)
	if id == 11 {
		return []amocrm.Subscription{{SubscriberID: 1, Type: "user"}}, nil
	}
	return []amocrm.Subscription{}, nil
}
func TestPermissionBatchOrdinaryGroupRoleAndPerLeadSubscription(t *testing.T) {
	install := uuid.New()
	group, role := int64(5), int64(9)
	base := &batchPolicyCRM{installation: install, crmFake: crmFake{users: map[int64]amocrm.DistributionUser{
		1: {ID: 1, Rights: amocrm.DistributionRights{IsActive: ptr(true), RoleID: &role, GroupID: &group}},
		2: {ID: 2, Rights: amocrm.DistributionRights{IsActive: ptr(true), GroupID: &group}},
	}, role: amocrm.DistributionRights{Leads: map[string]string{"view": "G"}}}}
	crm := &subscriberBatchCRM{batchPolicyCRM: base}
	result := CanViewLeads(context.Background(), crm, install, 1, []int64{10, 11, 12})
	for _, id := range []int64{10, 11, 12} {
		if !result[id].Allowed || result[id].Err != nil {
			t.Fatal(id, result)
		}
	}
	if base.usersRead.Load() != 2 || base.rolesRead.Load() != 1 || crm.subscriptionReads.Load() != 0 {
		t.Fatal("actor/owner role reuse incorrect")
	}
	// A stage deny overrides general group visibility; subscribers extend that
	// denial independently per lead, without reusing another lead's subscriber list.
	base.role.StatusRights = []amocrm.StatusRight{{EntityType: "leads", PipelineID: 20, StatusID: 30, Rights: map[string]string{"view": "D"}}}
	result = CanViewLeads(context.Background(), crm, install, 1, []int64{10, 11})
	if result[10].Allowed || !result[11].Allowed || result[10].Err != nil || result[11].Err != nil || crm.subscriptionReads.Load() != 2 {
		t.Fatal("stage/subscriber policy changed", result)
	}
	user := base.users[1]
	user.Rights.IsActive = ptr(false)
	base.users[1] = user
	result = CanViewLeads(context.Background(), crm, install, 1, []int64{10, 11})
	if result[10].Allowed || result[11].Allowed || result[11].Lead != nil {
		t.Fatal("disabled actor disclosed", result)
	}
}
