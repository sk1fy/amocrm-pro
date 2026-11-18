package amocrm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

// CompanyCRMUser deliberately preserves absent activity for the staff authority.
type CompanyCRMUser struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Rights struct {
		Active *bool `json:"is_active"`
	} `json:"rights"`
}

type CompanyCRMCompany struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Fields    []struct {
		ID     int64  `json:"field_id"`
		Type   string `json:"field_type"`
		Values []struct {
			Value json.RawMessage `json:"value"`
		} `json:"values"`
	} `json:"custom_fields_values"`
}

type CompanyCRMContact struct {
	ID        int64  `json:"id"`
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Embedded  struct {
		Companies []struct {
			ID int64 `json:"id"`
		} `json:"companies"`
	} `json:"_embedded"`
}

type companyCRMPage struct {
	Embedded map[string]json.RawMessage `json:"_embedded"`
	Links    struct {
		Next *struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
	empty bool
}

func (p *companyCRMPage) NoContent() { p.empty = true }

func companyCRMPages[T any](ctx context.Context, c *Client, id uuid.UUID, path, key string, validate func(T) error) ([]T, error) {
	result := []T{}
	for page := 1; page <= 40; page++ {
		var body companyCRMPage
		requestPath := fmt.Sprintf("%s?limit=250&page=%d", path, page)
		if key == "pipelines" {
			requestPath = path
		}
		if err := c.DoJSON(ctx, id, http.MethodGet, requestPath, nil, &body); err != nil {
			return nil, err
		}
		if body.empty {
			if key == "pipelines" {
				return nil, ErrIncompleteResponse
			}
			return result, nil
		}
		var rows []T
		raw, ok := body.Embedded[key]
		if !ok || json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) > 250 {
			return nil, ErrIncompleteResponse
		}
		if body.Links.Next != nil && body.Links.Next.Href == "" {
			return nil, ErrIncompleteResponse
		}
		next := body.Links.Next != nil
		if next && (len(rows) == 0 || key == "pipelines") {
			return nil, ErrIncompleteResponse
		}
		for _, row := range rows {
			if err := validate(row); err != nil {
				return nil, err
			}
			result = append(result, row)
		}
		if !next {
			return result, nil
		}
	}
	return nil, ErrIncompleteResponse
}

func (c *Client) VerifyCompanyCRMAccount(ctx context.Context, id uuid.UUID, account int64) error {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := c.DoJSON(ctx, id, http.MethodGet, "/api/v4/account", nil, &body); err != nil {
		return err
	}
	if account <= 0 || body.ID != account {
		return ErrIncompleteResponse
	}
	return nil
}
func (c *Client) CompanyCRMStaff(ctx context.Context, id uuid.UUID) ([]CompanyCRMUser, error) {
	seen := map[int64]bool{}
	return companyCRMPages(ctx, c, id, "/api/v4/users", "users", func(u CompanyCRMUser) error {
		if u.ID <= 0 || seen[u.ID] || len(u.Name) > 512 || len(u.Email) > 320 {
			return ErrIncompleteResponse
		}
		seen[u.ID] = true
		return nil
	})
}
func (c *Client) CompanyCRMCompanies(ctx context.Context, id uuid.UUID, account int64) ([]CompanyCRMCompany, error) {
	seen := map[int64]bool{}
	return companyCRMPages(ctx, c, id, "/api/v4/companies", "companies", func(u CompanyCRMCompany) error {
		if u.ID <= 0 || u.AccountID != account || seen[u.ID] || len(u.Name) > 512 {
			return ErrIncompleteResponse
		}
		seen[u.ID] = true
		return nil
	})
}
func (c *Client) CompanyCRMContacts(ctx context.Context, id uuid.UUID, account int64) ([]CompanyCRMContact, error) {
	seen := map[int64]bool{}
	return companyCRMPages(ctx, c, id, "/api/v4/contacts", "contacts", func(u CompanyCRMContact) error {
		if u.ID <= 0 || u.AccountID != account || seen[u.ID] || len(u.Name) > 512 {
			return ErrIncompleteResponse
		}
		for _, company := range u.Embedded.Companies {
			if company.ID <= 0 {
				return ErrIncompleteResponse
			}
		}
		seen[u.ID] = true
		return nil
	})
}

// CompanyCRMAccountIDs accepts only a verified numeric field and exact positive integers.
func (u CompanyCRMCompany) CompanyCRMAccountIDs(field int64) ([]int64, bool) {
	ids := []int64{}
	seen := map[int64]bool{}
	found := false
	for _, f := range u.Fields {
		if f.ID != field {
			continue
		}
		if found || f.Type != "numeric" {
			return nil, false
		}
		found = true
		for _, v := range f.Values {
			var value string
			if json.Unmarshal(v.Value, &value) != nil {
				value = string(v.Value)
			}
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 || seen[id] {
				return nil, false
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

type CompanyCRMField struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Enums []struct {
		ID    int64  `json:"id"`
		Value string `json:"value"`
	} `json:"enums"`
	RequiredStatuses []struct {
		PipelineID int64 `json:"pipeline_id"`
		StatusID   int64 `json:"status_id"`
	} `json:"required_statuses"`
}
type CompanyCRMStage struct {
	AccountID  int64  `json:"account_id"`
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	PipelineID int64  `json:"pipeline_id"`
	Type       int    `json:"type"`
}
type CompanyCRMPipeline struct {
	AccountID int64  `json:"account_id"`
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Embedded  struct {
		Statuses []CompanyCRMStage `json:"statuses"`
	} `json:"_embedded"`
}

func (c *Client) CompanyCRMFields(ctx context.Context, id uuid.UUID, entity string) ([]CompanyCRMField, error) {
	if entity != "companies" && entity != "leads" {
		return nil, ErrIncompleteResponse
	}
	seen := map[int64]bool{}
	return companyCRMPages(ctx, c, id, "/api/v4/"+entity+"/custom_fields", "custom_fields", func(f CompanyCRMField) error {
		if f.ID <= 0 || seen[f.ID] || f.Type == "" || len(f.Name) > 512 {
			return ErrIncompleteResponse
		}
		seen[f.ID] = true
		return nil
	})
}
func (c *Client) CompanyCRMPipelines(ctx context.Context, id uuid.UUID, account int64) ([]CompanyCRMPipeline, error) {
	seen := map[int64]bool{}
	return companyCRMPages(ctx, c, id, "/api/v4/leads/pipelines", "pipelines", func(p CompanyCRMPipeline) error {
		if p.ID <= 0 || p.AccountID != account || seen[p.ID] || p.Embedded.Statuses == nil || len(p.Embedded.Statuses) > 500 || len(p.Name) > 512 {
			return ErrIncompleteResponse
		}
		stages := map[int64]bool{}
		for _, s := range p.Embedded.Statuses {
			if s.ID <= 0 || s.PipelineID != p.ID || s.AccountID != account || stages[s.ID] || len(s.Name) > 512 {
				return ErrIncompleteResponse
			}
			stages[s.ID] = true
		}
		seen[p.ID] = true
		return nil
	})
}
