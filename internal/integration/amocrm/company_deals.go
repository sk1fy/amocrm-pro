package amocrm

import (
	"context"
	"net/http"
	"net/url"

	"github.com/google/uuid"
)

type companyLeadField struct {
	ID     int64                   `json:"field_id"`
	Values []companyLeadFieldValue `json:"values"`
}

type companyLeadFieldValue struct {
	Value any `json:"value"`
}

type companyLead struct {
	ID     int64              `json:"id"`
	Name   string             `json:"name"`
	Fields []companyLeadField `json:"custom_fields_values"`
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

func (c *Client) CreateCompanyLead(ctx context.Context, installation uuid.UUID, name string, pipeline, stage, markerFieldID int64, marker string) (int64, error) {
	type lead struct {
		Name     string             `json:"name"`
		Pipeline int64              `json:"pipeline_id"`
		Stage    int64              `json:"status_id"`
		Fields   []companyLeadField `json:"custom_fields_values,omitempty"`
	}
	input := lead{Name: name, Pipeline: pipeline, Stage: stage}
	if markerFieldID > 0 {
		input.Fields = []companyLeadField{{ID: markerFieldID, Values: []companyLeadFieldValue{{Value: marker}}}}
	}
	var result companyLeads
	err := c.DoJSON(ctx, installation, http.MethodPost, "/api/v4/leads", []lead{input}, &result)
	if err != nil {
		return 0, err
	}
	if len(result.Embedded.Leads) != 1 || result.Embedded.Leads[0].ID <= 0 {
		return 0, ErrIncompleteResponse
	}
	return result.Embedded.Leads[0].ID, nil
}

func (c *Client) FindCompanyLead(ctx context.Context, installation uuid.UUID, marker, title string, markerFieldID int64) (int64, error) {
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
		matches := lead.Name == title
		if markerFieldID > 0 {
			matches = lead.hasMarker(markerFieldID, marker)
		}
		if !matches {
			continue
		}
		if selected != 0 || lead.ID <= 0 {
			return 0, ErrIncompleteResponse
		}
		selected = lead.ID
	}
	return selected, nil
}

func (l companyLead) hasMarker(fieldID int64, marker string) bool {
	for _, field := range l.Fields {
		if field.ID == fieldID && len(field.Values) == 1 && field.Values[0].Value == marker {
			return true
		}
	}
	return false
}
