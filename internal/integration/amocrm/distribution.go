package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
)

// DistributionUser carries current resource rights only inside Core. API
// projections must exclude Rights, email and OAuth credentials.
type DistributionUser struct {
	ID     int64              `json:"id"`
	Name   string             `json:"name"`
	Rights DistributionRights `json:"rights"`
}
type DistributionRights struct {
	IsAdmin      bool              `json:"is_admin"`
	IsActive     *bool             `json:"is_active"`
	IsFree       bool              `json:"is_free"`
	GroupID      *int64            `json:"group_id"`
	RoleID       *int64            `json:"role_id"`
	Leads        map[string]string `json:"leads"`
	StatusRights []StatusRight     `json:"status_rights"`
}
type StatusRight struct {
	EntityType string            `json:"entity_type"`
	PipelineID int64             `json:"pipeline_id"`
	StatusID   int64             `json:"status_id"`
	Rights     map[string]string `json:"rights"`
}
type DistributionLead struct {
	ID                int64 `json:"id"`
	PipelineID        int64 `json:"pipeline_id"`
	StatusID          int64 `json:"status_id"`
	ResponsibleUserID int64 `json:"responsible_user_id"`
}
type Subscription struct {
	SubscriberID int64  `json:"subscriber_id"`
	Type         string `json:"type"`
}

func (c *Client) DistributionUser(ctx context.Context, id uuid.UUID, userID int64) (DistributionUser, error) {
	var u DistributionUser
	if userID <= 0 {
		return u, ErrIncompleteResponse
	}
	err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/users/%d", userID), nil, &u)
	if err == nil && (u.ID != userID || u.Rights.IsActive == nil) {
		err = ErrIncompleteResponse
	}
	return u, err
}
func (c *Client) DistributionRole(ctx context.Context, id uuid.UUID, roleID int64) (DistributionRights, error) {
	var r struct {
		ID     int64              `json:"id"`
		Rights DistributionRights `json:"rights"`
	}
	if roleID <= 0 {
		return r.Rights, ErrIncompleteResponse
	}
	err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/roles/%d", roleID), nil, &r)
	if err == nil && (r.ID != roleID || r.Rights.Leads == nil) {
		err = ErrIncompleteResponse
	}
	return r.Rights, err
}
func (c *Client) DistributionLead(ctx context.Context, id uuid.UUID, leadID int64) (DistributionLead, error) {
	var l DistributionLead
	if leadID <= 0 {
		return l, ErrIncompleteResponse
	}
	err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/leads/%d", leadID), nil, &l)
	if err == nil && (l.ID != leadID || l.PipelineID <= 0 || l.StatusID <= 0 || l.ResponsibleUserID <= 0) {
		err = ErrIncompleteResponse
	}
	return l, err
}
func (c *Client) DistributionUsers(ctx context.Context, id uuid.UUID) ([]DistributionUser, error) {
	users := []DistributionUser{}
	seen := map[int64]bool{}
	for page := 1; page <= 4; page++ {
		var r struct {
			Embedded struct {
				Users []DistributionUser `json:"users"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/users?limit=250&page=%d", page), nil, &r); err != nil {
			return nil, err
		}
		if r.Embedded.Users == nil || len(r.Embedded.Users) > 250 || r.Links.Next.Href != "" && len(r.Embedded.Users) == 0 {
			return nil, ErrIncompleteResponse
		}
		for _, u := range r.Embedded.Users {
			if u.ID <= 0 || u.Rights.IsActive == nil || len(u.Name) > 512 || seen[u.ID] {
				return nil, ErrIncompleteResponse
			}
			seen[u.ID] = true
			users = append(users, u)
		}
		if r.Links.Next.Href == "" {
			return users, nil
		}
	}
	return nil, ErrIncompleteResponse
}
func (c *Client) DistributionSubscriptions(ctx context.Context, id uuid.UUID, leadID int64) ([]Subscription, error) {
	result := []Subscription{}
	for page := 1; page <= 4; page++ {
		var r subscriptionPage
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/leads/%d/subscriptions?limit=250&page=%d", leadID, page), nil, &r); err != nil {
			return nil, err
		}
		if r.Embedded.Subscriptions == nil || len(r.Embedded.Subscriptions) > 250 || r.Links.Next.Href != "" && len(r.Embedded.Subscriptions) == 0 {
			return nil, ErrIncompleteResponse
		}
		for _, s := range r.Embedded.Subscriptions {
			if s.SubscriberID <= 0 || s.Type != "user" && s.Type != "group" {
				return nil, ErrIncompleteResponse
			}
			result = append(result, s)
		}
		if r.Links.Next.Href == "" {
			return result, nil
		}
	}
	return nil, ErrIncompleteResponse
}

// DistributionPipelines reads a complete bounded snapshot. Next URLs are never
// followed: only locally constructed numbered pages under the allowlisted API.
func (c *Client) DistributionPipelines(ctx context.Context, id uuid.UUID) ([]Pipeline, error) {
	out := []Pipeline{}
	seen := map[int64]bool{}
	for page := 1; page <= 4; page++ {
		var r struct {
			Embedded struct {
				Pipelines []struct {
					ID       int64  `json:"id"`
					Name     string `json:"name"`
					Embedded struct {
						Statuses []struct {
							ID   int64  `json:"id"`
							Name string `json:"name"`
						} `json:"statuses"`
					} `json:"_embedded"`
				} `json:"pipelines"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/leads/pipelines?limit=50&page=%d", page), nil, &r); err != nil {
			return nil, err
		}
		if r.Embedded.Pipelines == nil || len(r.Embedded.Pipelines) > 50 || r.Links.Next.Href != "" && len(r.Embedded.Pipelines) == 0 {
			return nil, ErrIncompleteResponse
		}
		for _, p := range r.Embedded.Pipelines {
			if p.Embedded.Statuses == nil || p.ID <= 0 || seen[p.ID] || len(p.Name) > 512 || len(p.Embedded.Statuses) > 200 {
				return nil, ErrIncompleteResponse
			}
			seen[p.ID] = true
			statuses := []PipelineStatus{}
			sid := map[int64]bool{}
			for _, s := range p.Embedded.Statuses {
				if s.ID <= 0 || sid[s.ID] || len(s.Name) > 512 {
					return nil, ErrIncompleteResponse
				}
				sid[s.ID] = true
				statuses = append(statuses, PipelineStatus{s.ID, s.Name})
			}
			out = append(out, Pipeline{p.ID, p.Name, statuses})
		}
		if r.Links.Next.Href == "" {
			return out, nil
		}
	}
	return nil, ErrIncompleteResponse
}

type subscriptionPage struct {
	Embedded struct {
		Subscriptions []Subscription `json:"subscriptions"`
	} `json:"_embedded"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

func (p *subscriptionPage) NoContent() { p.Embedded.Subscriptions = []Subscription{} }
