package amocrm

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

type companyLead struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type companyLeads struct {
	Embedded struct {
		Leads []companyLead `json:"leads"`
	} `json:"_embedded"`
	Links struct {
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
	empty bool
}

func (l *companyLeads) NoContent() { l.empty = true }

func (c *Client) CreateCompanyLead(ctx context.Context, installation uuid.UUID, name string, pipeline, stage int64) (int64, error) {
	type lead struct {
		Name     string `json:"name"`
		Pipeline int64  `json:"pipeline_id"`
		Stage    int64  `json:"status_id"`
	}
	var result companyLeads
	err := c.DoJSON(ctx, installation, http.MethodPost, "/api/v4/leads", []lead{{name, pipeline, stage}}, &result)
	if err != nil {
		return 0, err
	}
	if len(result.Embedded.Leads) != 1 || result.Embedded.Leads[0].ID <= 0 {
		return 0, ErrIncompleteResponse
	}
	return result.Embedded.Leads[0].ID, nil
}

func (c *Client) FindCompanyLead(ctx context.Context, installation uuid.UUID, marker, title string) (int64, error) {
	var result companyLeads
	params := url.Values{"query": {marker}, "limit": {"250"}}
	if err := c.DoJSON(ctx, installation, http.MethodGet, "/api/v4/leads?"+params.Encode(), nil, &result); err != nil {
		return 0, err
	}
	if result.empty {
		return 0, nil
	}
	if result.Embedded.Leads == nil || result.Links.Next != nil {
		return 0, ErrIncompleteResponse
	}
	var selected int64
	for _, lead := range result.Embedded.Leads {
		if lead.Name == title {
			if selected != 0 || lead.ID <= 0 {
				return 0, ErrIncompleteResponse
			}
			selected = lead.ID
		}
	}
	return selected, nil
}
