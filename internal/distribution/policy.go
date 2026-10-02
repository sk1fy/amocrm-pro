package distribution

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type CRM interface {
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
	view := rights.Leads["view"]
	var allowed bool
	switch view {
	case "A":
		allowed = true
	case "D":
		allowed = false
	case "M":
		allowed = l.ResponsibleUserID == actor
	case "G":
		if u.Rights.GroupID == nil {
			return false, ErrUnavailable
		}
		owner, e := crm.DistributionUser(ctx, install, l.ResponsibleUserID)
		if e != nil || owner.ID != l.ResponsibleUserID || owner.Rights.GroupID == nil {
			return false, ErrUnavailable
		}
		allowed = *owner.Rights.GroupID == *u.Rights.GroupID
	default:
		return false, ErrUnavailable
	}
	matched := false
	for _, r := range rights.StatusRights {
		if r.EntityType == "leads" && r.PipelineID == l.PipelineID && r.StatusID == l.StatusID {
			if matched {
				return false, ErrUnavailable
			}
			matched = true
			switch r.Rights["view"] {
			case "A":
				allowed = true
			case "D":
				allowed = false
			default:
				return false, ErrUnavailable
			}
		}
	}
	if allowed {
		return true, nil
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
