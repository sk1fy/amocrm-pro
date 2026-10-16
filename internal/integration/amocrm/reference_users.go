package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
)

// CRMUser carries current resource rights only inside Core. API
// projections must exclude Rights, email and OAuth credentials.
type CRMUser struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	Rights CRMRights `json:"rights"`
}
type CRMRights struct {
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

// CRMUsers is the product-neutral complete bounded reader.
func (c *Client) CRMUsers(ctx context.Context, id uuid.UUID) ([]CRMUser, error) {
	users := []CRMUser{}
	seen := map[int64]bool{}
	for page := 1; page <= 4; page++ {
		var r struct {
			Embedded struct {
				Users []CRMUser `json:"users"`
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
