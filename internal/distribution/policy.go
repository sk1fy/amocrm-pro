package distribution

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type CRM interface {
	DistributionAccountTimezone(context.Context, uuid.UUID, int64) (string, error)
	DistributionUser(context.Context, uuid.UUID, int64) (amocrm.DistributionUser, error)
	DistributionRole(context.Context, uuid.UUID, int64) (amocrm.DistributionRights, error)
	DistributionLead(context.Context, uuid.UUID, int64) (amocrm.DistributionLead, error)
	DistributionUsers(context.Context, uuid.UUID) ([]amocrm.DistributionUser, error)
	DistributionSubscriptions(context.Context, uuid.UUID, int64) ([]amocrm.Subscription, error)
	DistributionPipelines(context.Context, uuid.UUID) ([]amocrm.Pipeline, error)
}

// CanViewLead implements the documented order: general rights, status override,
// then subscriber extension. Unknown values and incomplete sources fail closed.
func CanViewLead(ctx context.Context, crm CRM, install uuid.UUID, actor, leadID int64) (bool, error) {
	return leadRight(ctx, crm, install, actor, leadID, "view")
}

func leadRight(ctx context.Context, crm CRM, install uuid.UUID, actor, leadID int64, action string) (bool, error) {
	u, err := crm.DistributionUser(ctx, install, actor)
	if err != nil {
		return false, ErrUnavailable
	}
	if u.ID != actor || u.Rights.IsActive == nil {
		return false, ErrUnavailable
	}
	if u.Rights.IsFree {
		return false, ErrUnavailable
	}
	if !*u.Rights.IsActive {
		return false, nil
	}
	l, err := crm.DistributionLead(ctx, install, leadID)
	if err != nil {
		return false, ErrUnavailable
	}
	if l.ID != leadID || l.PipelineID <= 0 || l.StatusID <= 0 || l.ResponsibleUserID <= 0 {
		return false, ErrUnavailable
	}
	if u.Rights.IsAdmin {
		return true, nil
	}
	rights := u.Rights
	if rights.RoleID != nil && *rights.RoleID > 0 {
		r, e := crm.DistributionRole(ctx, install, *rights.RoleID)
		if e != nil {
			return false, ErrUnavailable
		}
		rights.Leads = r.Leads
		rights.StatusRights = r.StatusRights
	}
	evaluate := func(level string) (bool, error) {
		switch level {
		case "A":
			return true, nil
		case "D":
			return false, nil
		case "M":
			return l.ResponsibleUserID == actor, nil
		case "G":
			if u.Rights.GroupID == nil {
				return false, ErrUnavailable
			}
			owner, e := crm.DistributionUser(ctx, install, l.ResponsibleUserID)
			if e != nil || owner.ID != l.ResponsibleUserID || owner.Rights.GroupID == nil {
				return false, ErrUnavailable
			}
			return *owner.Rights.GroupID == *u.Rights.GroupID, nil
		default:
			return false, ErrUnavailable
		}
	}

	allowed, err := evaluate(rights.Leads[action])
	if err != nil {
		return false, err
	}
	matched := false
	for _, r := range rights.StatusRights {
		if r.EntityType != "leads" || r.PipelineID != l.PipelineID || r.StatusID != l.StatusID {
			continue
		}
		level := r.Rights[action]
		if matched || (action == "view" && level != "A" && level != "D") {
			return false, ErrUnavailable
		}
		matched = true
		allowed, err = evaluate(level)
		if err != nil {
			return false, err
		}
	}
	if allowed || action != "view" {
		return allowed, nil
	}
	subscriptions, err := crm.DistributionSubscriptions(ctx, install, leadID)
	if err != nil {
		return false, ErrUnavailable
	}
	for _, s := range subscriptions {
		if s.Type == "user" && s.SubscriberID == actor || s.Type == "group" && u.Rights.GroupID != nil && s.SubscriberID == *u.Rights.GroupID {
			return true, nil
		}
	}
	return false, nil
}
