package distribution

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"testing"
)

type crmFake struct {
	users       map[int64]amocrm.DistributionUser
	lead        amocrm.DistributionLead
	role        amocrm.DistributionRights
	subs        []amocrm.Subscription
	err         error
	pipelines   []amocrm.Pipeline
	timezone    string
	timezoneErr error
}

func (f *crmFake) DistributionAccountTimezone(context.Context, uuid.UUID, int64) (string, error) {
	if f.timezoneErr != nil {
		return "", f.timezoneErr
	}
	if f.timezone != "" {
		return f.timezone, nil
	}
	return "UTC", f.err
}

func (f *crmFake) DistributionUser(_ context.Context, _ uuid.UUID, id int64) (amocrm.DistributionUser, error) {
	return f.users[id], f.err
}
func (f *crmFake) DistributionRole(context.Context, uuid.UUID, int64) (amocrm.DistributionRights, error) {
	return f.role, f.err
}
func (f *crmFake) DistributionLead(context.Context, uuid.UUID, int64) (amocrm.DistributionLead, error) {
	return f.lead, f.err
}
func (f *crmFake) DistributionUsers(context.Context, uuid.UUID) ([]amocrm.DistributionUser, error) {
	r := []amocrm.DistributionUser{}
	for _, u := range f.users {
		r = append(r, u)
	}
	return r, f.err
}
func (f *crmFake) DistributionSubscriptions(context.Context, uuid.UUID, int64) ([]amocrm.Subscription, error) {
	return f.subs, f.err
}
func (f *crmFake) DistributionPipelines(context.Context, uuid.UUID) ([]amocrm.Pipeline, error) {
	return f.pipelines, f.err
}
func ptr[T any](v T) *T { return &v }
func TestCurrentLeadPolicy(t *testing.T) {
	cases := []struct {
		name, view    string
		owner         int64
		group         int64
		override      string
		subscriber    string
		active, admin bool
		unknown, role bool
		want          bool
		wantError     bool
	}{
		{name: "all", view: "A", active: true, want: true}, {name: "own", view: "M", owner: 1, active: true, want: true}, {name: "other", view: "M", owner: 2, active: true},
		{name: "same group", view: "G", owner: 2, group: 5, active: true, want: true}, {name: "other group", view: "G", owner: 2, group: 8, active: true},
		{name: "stage denies all", view: "A", override: "D", active: true}, {name: "stage extends deny", view: "D", override: "A", active: true, want: true},
		{name: "user subscription", view: "D", subscriber: "user", active: true, want: true}, {name: "group subscription", view: "D", subscriber: "group", active: true, want: true},
		{name: "unknown", view: "?", active: true, wantError: true}, {name: "missing activity", view: "A", unknown: true, wantError: true},
		{name: "disabled admin", view: "A", admin: true}, {name: "admin", view: "D", admin: true, active: true, want: true},
		{name: "role effective rights", view: "D", active: true, role: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			active := ptr(c.active)
			if c.unknown {
				active = nil
			}
			u := amocrm.DistributionUser{ID: 1, Rights: amocrm.DistributionRights{IsActive: active, IsAdmin: c.admin, GroupID: ptr(int64(5)), Leads: map[string]string{"view": c.view}}}
			f := &crmFake{users: map[int64]amocrm.DistributionUser{1: u, 2: {ID: 2, Rights: amocrm.DistributionRights{GroupID: ptr(c.group)}}}, lead: amocrm.DistributionLead{ID: 10, PipelineID: 20, StatusID: 30, ResponsibleUserID: c.owner}}
			if f.lead.ResponsibleUserID == 0 {
				f.lead.ResponsibleUserID = 2
			}
			if c.override != "" {
				u.Rights.StatusRights = []amocrm.StatusRight{{EntityType: "leads", PipelineID: 20, StatusID: 30, Rights: map[string]string{"view": c.override}}}
			}
			if c.role {
				u.Rights.RoleID = ptr(int64(9))
				f.role = amocrm.DistributionRights{Leads: map[string]string{"view": "A"}}
			}
			f.users[1] = u
			if c.subscriber != "" {
				id := int64(1)
				if c.subscriber == "group" {
					id = 5
				}
				f.subs = []amocrm.Subscription{{Type: c.subscriber, SubscriberID: id}}
			}
			got, e := CanViewLead(context.Background(), f, uuid.New(), 1, 10)
			if got != c.want || (e != nil) != c.wantError {
				t.Fatalf("got%v/%v want%v/error%v", got, e, c.want, c.wantError)
			}
		})
	}
	f := &crmFake{err: errors.New("CRM unavailable")}
	if got, e := CanViewLead(context.Background(), f, uuid.New(), 1, 10); got || !errors.Is(e, ErrUnavailable) {
		t.Fatal("source failure grants access")
	}
}
