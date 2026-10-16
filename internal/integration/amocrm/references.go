package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/crmreferences"
)

// CRMReferences uses the same OAuth provider and account limiter as all existing
// CRM reads. Account groups are authoritative, including groups without users.
func (c *Client) CRMReferences(ctx context.Context, installation uuid.UUID, accountID int64) (crmreferences.Data, error) {
	out := crmreferences.Data{Users: []crmreferences.User{}, Groups: []crmreferences.Group{}, Pipelines: []crmreferences.Pipeline{}, Customers: crmreferences.Customers{Availability: "unknown", Statuses: []crmreferences.Status{}}}
	var account struct {
		ID            int64  `json:"id"`
		CustomersMode string `json:"customers_mode"`
		Embedded      struct {
			Groups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"users_groups"`
		} `json:"_embedded"`
	}
	if err := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/account?with=users_groups", nil, &account); err != nil {
		return out, referenceError(err)
	}
	if account.ID != accountID || account.Embedded.Groups == nil {
		return out, crmreferences.ErrIncomplete
	}
	if len(account.Embedded.Groups) > 1000 {
		return out, crmreferences.ErrTooLarge
	}
	for _, g := range account.Embedded.Groups {
		out.Groups = append(out.Groups, crmreferences.Group{ID: strconv.FormatInt(g.ID, 10), Name: g.Name})
	}
	users, err := c.CRMUsers(ctx, installation)
	if err != nil {
		return out, referenceError(err)
	}
	for _, u := range users {
		var group *string
		if u.Rights.GroupID != nil && *u.Rights.GroupID != 0 {
			v := strconv.FormatInt(*u.Rights.GroupID, 10)
			group = &v
		}
		out.Users = append(out.Users, crmreferences.User{ID: strconv.FormatInt(u.ID, 10), Name: u.Name, IsActive: *u.Rights.IsActive, GroupID: group})
	}
	out.Pipelines, err = c.CRMReferencePipelines(ctx, installation, accountID)
	if err != nil {
		return out, err
	}
	out.Customers.CheckedAt = time.Now().UTC()
	switch account.CustomersMode {
	case "unavailable", "disabled":
		out.Customers.Availability = "unsupported"
	case "segments", "dynamic", "periodicity":
		statuses, e := c.crmCustomerStatuses(ctx, installation, accountID)
		if e == nil {
			out.Customers.Availability = "supported"
			out.Customers.Statuses = statuses
		} else {
			code := "references_unavailable"
			if errors.Is(e, crmreferences.ErrIncomplete) {
				code = "references_incomplete"
			}
			if errors.Is(e, crmreferences.ErrTooLarge) {
				return out, e
			}
			out.Customers.ErrorCode = &code
		}
	default:
		code := "customers_capability_unknown"
		out.Customers.ErrorCode = &code
	}
	return out, nil
}
func referenceError(err error) error {
	if errors.Is(err, ErrResponseTooLarge) {
		return errors.Join(crmreferences.ErrTooLarge, err)
	}
	if errors.Is(err, ErrIncompleteResponse) {
		return errors.Join(crmreferences.ErrIncomplete, err)
	}
	return errors.Join(crmreferences.ErrUnavailable, err)
}

type referenceStatus struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Sort       int     `json:"sort"`
	Color      *string `json:"color"`
	PipelineID *int64  `json:"pipeline_id"`
	AccountID  *int64  `json:"account_id"`
}

func (s referenceStatus) dto() crmreferences.Status {
	return crmreferences.Status{ID: strconv.FormatInt(s.ID, 10), Name: s.Name, Sort: s.Sort, Color: s.Color}
}

// Embedded statuses are the API's full pipeline child collection; a next link
// would make that claim false and is rejected instead of returning partial rows.
func (c *Client) CRMReferencePipelines(ctx context.Context, installation uuid.UUID, accountID int64) ([]crmreferences.Pipeline, error) {
	out := []crmreferences.Pipeline{}
	size := 0
	seen := map[int64]bool{}
	for page := 1; page <= 20; page++ {
		var r struct {
			Embedded struct {
				Pipelines []struct {
					ID        int64  `json:"id"`
					Name      string `json:"name"`
					Sort      int    `json:"sort"`
					AccountID *int64 `json:"account_id"`
					Embedded  struct {
						Statuses []referenceStatus `json:"statuses"`
					} `json:"_embedded"`
					Links struct {
						Next struct {
							Href string `json:"href"`
						} `json:"next"`
					} `json:"_links"`
				} `json:"pipelines"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, installation, http.MethodGet, fmt.Sprintf("/api/v4/leads/pipelines?limit=50&page=%d", page), nil, &r); err != nil {
			return nil, referenceError(err)
		}
		rows := r.Embedded.Pipelines
		if rows == nil || len(rows) > 50 || len(rows) == 0 && r.Links.Next.Href != "" {
			return nil, crmreferences.ErrIncomplete
		}
		for _, p := range rows {
			if p.ID <= 0 || seen[p.ID] || p.Embedded.Statuses == nil || accountID > 0 && p.AccountID != nil && *p.AccountID != accountID || p.Links.Next.Href != "" {
				return nil, crmreferences.ErrIncomplete
			}
			seen[p.ID] = true
			if len(p.Embedded.Statuses) > 1000 {
				return nil, crmreferences.ErrTooLarge
			}
			statuses := []crmreferences.LeadStatus{}
			for _, s := range p.Embedded.Statuses {
				if s.PipelineID != nil && *s.PipelineID != p.ID || accountID > 0 && s.AccountID != nil && *s.AccountID != accountID {
					return nil, crmreferences.ErrIncomplete
				}
				kind := "open"
				if s.ID == 142 {
					kind = "won"
				}
				if s.ID == 143 {
					kind = "lost"
				}
				statuses = append(statuses, crmreferences.LeadStatus{Status: s.dto(), Kind: kind})
			}
			row := crmreferences.Pipeline{ID: strconv.FormatInt(p.ID, 10), Name: p.Name, Sort: p.Sort, Statuses: statuses}
			encoded, _ := json.Marshal(row)
			size += len(encoded)
			if size > crmreferences.MaxBytes {
				return nil, crmreferences.ErrTooLarge
			}
			out = append(out, row)
		}
		if r.Links.Next.Href == "" {
			return out, nil
		}
	}
	return nil, crmreferences.ErrTooLarge
}
func (c *Client) crmCustomerStatuses(ctx context.Context, installation uuid.UUID, accountID int64) ([]crmreferences.Status, error) {
	out := []crmreferences.Status{}
	seen := map[int64]bool{}
	for page := 1; page <= 4; page++ {
		var r struct {
			Embedded struct {
				Statuses []referenceStatus `json:"statuses"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, installation, http.MethodGet, fmt.Sprintf("/api/v4/customers/statuses?limit=250&page=%d", page), nil, &r); err != nil {
			return nil, referenceError(err)
		}
		if r.Embedded.Statuses == nil || len(r.Embedded.Statuses) > 250 || len(r.Embedded.Statuses) == 0 && r.Links.Next.Href != "" {
			return nil, crmreferences.ErrIncomplete
		}
		for _, s := range r.Embedded.Statuses {
			if s.ID <= 0 || seen[s.ID] || accountID > 0 && s.AccountID != nil && *s.AccountID != accountID {
				return nil, crmreferences.ErrIncomplete
			}
			seen[s.ID] = true
			out = append(out, s.dto())
		}
		if r.Links.Next.Href == "" {
			return out, nil
		}
	}
	return nil, crmreferences.ErrTooLarge
}
