package amocrm

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
)

type ProblemLead struct {
	ID                int64 `json:"id"`
	ResponsibleUserID int64 `json:"responsible_user_id"`
	PipelineID        int64 `json:"pipeline_id"`
	StatusID          int64 `json:"status_id"`
	IsDeleted         bool  `json:"is_deleted"`
}
type ProblemTask struct {
	ID                int64  `json:"id"`
	ResponsibleUserID int64  `json:"responsible_user_id"`
	EntityID          int64  `json:"entity_id"`
	EntityType        string `json:"entity_type"`
	IsCompleted       bool   `json:"is_completed"`
	CompleteTill      int64  `json:"complete_till"`
}

type problemLeadsPage struct {
	Embedded *struct {
		Leads []ProblemLead `json:"leads"`
	} `json:"_embedded"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

func (p *problemLeadsPage) NoContent() {
	p.Embedded = &struct {
		Leads []ProblemLead `json:"leads"`
	}{Leads: []ProblemLead{}}
}

type problemTasksPage struct {
	Embedded *struct {
		Tasks []ProblemTask `json:"tasks"`
	} `json:"_embedded"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

func (p *problemTasksPage) NoContent() {
	p.Embedded = &struct {
		Tasks []ProblemTask `json:"tasks"`
	}{Tasks: []ProblemTask{}}
}

// Each call reads one bounded page. Collection checkpoints and publication
// are owned by the durable Gateway snapshot worker, not an account-size limit.
type ProblemPage struct {
	Leads   []ProblemLead
	Tasks   []ProblemTask
	HasNext bool
}

func (c *Client) ReadProblemPage(ctx context.Context, id uuid.UUID, phase string, page int) (ProblemPage, error) {
	if page < 1 {
		return ProblemPage{}, ErrIncompleteResponse
	}
	switch phase {
	case "leads":
		var response problemLeadsPage
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/leads?limit=250&page=%d&order[id]=asc", page), nil, &response); err != nil {
			return ProblemPage{}, err
		}
		if response.Embedded == nil || response.Embedded.Leads == nil || len(response.Embedded.Leads) > 250 || len(response.Embedded.Leads) == 0 && response.Links.Next.Href != "" {
			return ProblemPage{}, ErrIncompleteResponse
		}
		for _, r := range response.Embedded.Leads {
			if r.ID <= 0 {
				return ProblemPage{}, ErrIncompleteResponse
			}
		}
		return ProblemPage{Leads: response.Embedded.Leads, HasNext: response.Links.Next.Href != ""}, nil
	case "tasks":
		var response problemTasksPage
		if err := c.DoJSON(ctx, id, http.MethodGet, fmt.Sprintf("/api/v4/tasks?filter[is_completed]=0&limit=250&page=%d&order[id]=asc", page), nil, &response); err != nil {
			return ProblemPage{}, err
		}
		if response.Embedded == nil || response.Embedded.Tasks == nil || len(response.Embedded.Tasks) > 250 || len(response.Embedded.Tasks) == 0 && response.Links.Next.Href != "" {
			return ProblemPage{}, ErrIncompleteResponse
		}
		for _, r := range response.Embedded.Tasks {
			if r.ID <= 0 {
				return ProblemPage{}, ErrIncompleteResponse
			}
		}
		return ProblemPage{Tasks: response.Embedded.Tasks, HasNext: response.Links.Next.Href != ""}, nil
	}
	return ProblemPage{}, ErrIncompleteResponse
}
