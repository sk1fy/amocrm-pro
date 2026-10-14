package amocrm

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
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
	Name              string `json:"name"`
	ID                int64  `json:"id"`
	PipelineID        int64  `json:"pipeline_id"`
	StatusID          int64  `json:"status_id"`
	ResponsibleUserID int64  `json:"responsible_user_id"`
}
type Subscription struct {
	SubscriberID int64  `json:"subscriber_id"`
	Type         string `json:"type"`
}

// DistributionAccountTimezone reads the account behind the installation's
// credentials and verifies its identity before exposing its time zone.
func (c *Client) DistributionAccountTimezone(ctx context.Context, id uuid.UUID, accountID int64) (string, error) {
	if accountID <= 0 {
		return "", ErrIncompleteResponse
	}
	var account struct {
		ID       int64 `json:"id"`
		Embedded struct {
			DateTime struct {
				Timezone string `json:"timezone"`
			} `json:"datetime_settings"`
		} `json:"_embedded"`
	}
	if err := c.DoJSON(ctx, id, http.MethodGet, "/api/v4/account?with=datetime_settings", nil, &account); err != nil {
		return "", err
	}
	tz := account.Embedded.DateTime.Timezone
	if account.ID != accountID || tz == "" || tz == "Local" || len(tz) > 128 {
		return "", ErrIncompleteResponse
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "", ErrIncompleteResponse
	}
	return tz, nil
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

// DistributionLeads reads only the requested IDs in one bounded CRM request.
// Missing, duplicated or foreign rows never become positive permission evidence.
func (c *Client) DistributionLeads(ctx context.Context, installation uuid.UUID, ids []int64) (map[int64]DistributionLead, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, ErrIncompleteResponse
	}
	requested := make(map[int64]bool, len(ids))
	query := url.Values{"limit": {"100"}, "page": {"1"}}
	for _, id := range ids {
		if id <= 0 || requested[id] {
			return nil, ErrIncompleteResponse
		}
		requested[id] = true
		query.Add("filter[id][]", strconv.FormatInt(id, 10))
	}
	if len(ids) == 1 {
		lead, err := c.DistributionLead(ctx, installation, ids[0])
		if err != nil {
			return nil, err
		}
		return map[int64]DistributionLead{ids[0]: lead}, nil
	}
	var result struct {
		Embedded struct {
			Leads []DistributionLead `json:"leads"`
		} `json:"_embedded"`
		Links struct {
			Next struct {
				Href string `json:"href"`
			} `json:"next"`
		} `json:"_links"`
	}
	if err := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/leads?"+query.Encode(), nil, &result); err != nil {
		// Exact-ID filters are not enabled on all amoCRM accounts. Only explicit
		// unsupported-filter/payment responses use the old exact-ID reads. Never
		// retry an authorization failure, timeout, rate limit or incomplete response.
		var api *APIError
		if !errors.As(err, &api) || (api.StatusCode != 400 && api.StatusCode != 402 && api.StatusCode != 422) {
			return nil, err
		}
		rows := make([]DistributionLead, len(ids))
		errs := make([]error, len(ids))
		jobs := make(chan int, len(ids))
		for i := range ids {
			jobs <- i
		}
		close(jobs)
		var workers sync.WaitGroup
		for n := 0; n < 4 && n < len(ids); n++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for i := range jobs {
					if e := ctx.Err(); e != nil {
						errs[i] = e
						continue
					}
					rows[i], errs[i] = c.DistributionLead(ctx, installation, ids[i])
				}
			}()
		}
		workers.Wait()
		leads := make(map[int64]DistributionLead, len(ids))
		for i, id := range ids {
			if errs[i] != nil {
				return nil, errs[i]
			}
			leads[id] = rows[i]
		}
		return leads, nil
	}
	if result.Embedded.Leads == nil || result.Links.Next.Href != "" || len(result.Embedded.Leads) > len(ids) {
		return nil, ErrIncompleteResponse
	}
	leads := make(map[int64]DistributionLead, len(ids))
	for _, lead := range result.Embedded.Leads {
		if !requested[lead.ID] || leads[lead.ID].ID != 0 || lead.PipelineID <= 0 || lead.StatusID <= 0 || lead.ResponsibleUserID <= 0 {
			return nil, ErrIncompleteResponse
		}
		leads[lead.ID] = lead
	}
	return leads, nil
}
