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

type CompanyLeadField struct {
	ID    int64
	Value string
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

func (c *Client) CreateCompanyLead(ctx context.Context, installation uuid.UUID, name string, pipeline, stage, responsible int64, fields []CompanyLeadField) (int64, error) {
	type lead struct {
		Responsible int64              `json:"responsible_user_id,omitempty"`
		Name        string             `json:"name"`
		Pipeline    int64              `json:"pipeline_id"`
		Stage       int64              `json:"status_id"`
		Fields      []companyLeadField `json:"custom_fields_values,omitempty"`
	}
	input := lead{Name: name, Pipeline: pipeline, Stage: stage, Responsible: responsible}
	for _, field := range fields {
		input.Fields = append(input.Fields, companyLeadField{ID: field.ID, Values: []companyLeadFieldValue{{Value: field.Value}}})
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
			matches = lead.hasFieldValue(markerFieldID, marker)
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

func (l companyLead) hasFieldValue(fieldID int64, value string) bool {
	for _, field := range l.Fields {
		if field.ID == fieldID && len(field.Values) == 1 && field.Values[0].Value == value {
			return true
		}
	}
	return false
}
