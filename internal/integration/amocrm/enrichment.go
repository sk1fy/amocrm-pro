package amocrm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

const (
	enrichmentBatchLimit    = 50
	enrichmentPayloadLimit  = 32768
	enrichmentNameLimit     = 512
	enrichmentFieldPageSize = 50
	enrichmentFieldPages    = 20
	enrichmentFieldLimit    = 1000
	enrichmentPipelineLimit = 50
	enrichmentStatusLimit   = 200
)

type Note struct {
	Invalid                            bool // Known ID with rejected content; never expose a truncated object.
	ID, EntityID, CreatedBy, UpdatedAt int64
	EntityType, NoteType               string
	Params                             json.RawMessage
}

type Task struct {
	Invalid                                                              bool // Known ID with rejected content; never expose a truncated object.
	ID, EntityID, ResponsibleUserID, CompleteTill, TaskTypeID, UpdatedAt int64
	EntityType, Text, ResultText                                         string
	IsCompleted                                                          bool
}

type PipelineStatus struct {
	ID   int64
	Name string
}

type Pipeline struct {
	ID       int64
	Name     string
	Statuses []PipelineStatus
}

type CustomFieldEnum struct {
	ID    int64
	Value string
}

type CustomField struct {
	ID         int64
	Name       string
	Type       string
	EntityType string
	Enums      []CustomFieldEnum
}

type EntityName struct {
	Invalid    bool // Known ID with rejected content; never expose a truncated object.
	ID         int64
	EntityType string
	Name       string
}

func catalogEntityType(kind string) (string, error) {
	switch kind {
	case "lead", "leads":
		return "leads", nil
	case "contact", "contacts":
		return "contacts", nil
	case "company", "companies":
		return "companies", nil
	case "customer", "customers":
		return "customers", nil
	default:
		return "", errors.New("unsupported amoCRM entity type")
	}
}

func validateIDBatch(ids []int64) error {
	if len(ids) == 0 || len(ids) > enrichmentBatchLimit {
		return errors.New("id batch is empty or exceeds the enrichment limit")
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return errors.New("ids must be distinct and positive")
		}
		if _, ok := seen[id]; ok {
			return errors.New("ids must be distinct and positive")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func requestedIDs(ids []int64) map[int64]struct{} {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	return seen
}

func idFilterQuery(ids []int64) string {
	q := url.Values{"limit": {strconv.Itoa(enrichmentBatchLimit)}}
	for i, id := range ids {
		q.Set("filter[id]["+strconv.Itoa(i)+"]", strconv.FormatInt(id, 10))
	}
	return q.Encode()
}

func rejectIncompletePage(n, limit int, next string) error {
	if n > limit || (next != "" && n == 0) {
		return ErrIncompleteResponse
	}
	return nil
}

func (c *Client) ListNotes(ctx context.Context, installationID uuid.UUID, entityType string, ids []int64) ([]Note, error) {
	plural, err := catalogEntityType(entityType)
	if err != nil {
		return nil, err
	}
	if err := validateIDBatch(ids); err != nil {
		return nil, err
	}
	var result struct {
		Embedded struct {
			Notes []struct {
				ID        int64           `json:"id"`
				EntityID  int64           `json:"entity_id"`
				CreatedBy int64           `json:"created_by"`
				UpdatedAt int64           `json:"updated_at"`
				NoteType  string          `json:"note_type"`
				Params    json.RawMessage `json:"params"`
			} `json:"notes"`
		} `json:"_embedded"`
		Links struct {
			Next struct {
				Href string `json:"href"`
			} `json:"next"`
		} `json:"_links"`
	}
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/"+plural+"/notes?"+idFilterQuery(ids), nil, &result); err != nil {
		return nil, err
	}
	if err := rejectIncompletePage(len(result.Embedded.Notes), enrichmentBatchLimit, result.Links.Next.Href); err != nil {
		return nil, err
	}
	want := requestedIDs(ids)
	notes := make([]Note, 0, len(result.Embedded.Notes))
	for _, item := range result.Embedded.Notes {
		if _, ok := want[item.ID]; !ok {
			return nil, ErrIncompleteResponse
		}
		if len(item.Params) > enrichmentPayloadLimit {
			notes = append(notes, Note{ID: item.ID, Invalid: true})
			continue
		}
		notes = append(notes, Note{
			ID: item.ID, EntityID: item.EntityID, CreatedBy: item.CreatedBy, UpdatedAt: item.UpdatedAt,
			EntityType: plural, NoteType: item.NoteType, Params: item.Params,
		})
	}
	return notes, nil
}

func (c *Client) ListTasks(ctx context.Context, installationID uuid.UUID, ids []int64) ([]Task, error) {
	if err := validateIDBatch(ids); err != nil {
		return nil, err
	}
	var result struct {
		Embedded struct {
			Tasks []struct {
				ID                int64           `json:"id"`
				EntityID          int64           `json:"entity_id"`
				ResponsibleUserID int64           `json:"responsible_user_id"`
				CompleteTill      int64           `json:"complete_till"`
				TaskTypeID        int64           `json:"task_type_id"`
				UpdatedAt         int64           `json:"updated_at"`
				EntityType        string          `json:"entity_type"`
				Text              string          `json:"text"`
				IsCompleted       bool            `json:"is_completed"`
				Result            json.RawMessage `json:"result"`
			} `json:"tasks"`
		} `json:"_embedded"`
		Links struct {
			Next struct {
				Href string `json:"href"`
			} `json:"next"`
		} `json:"_links"`
	}
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/tasks?"+idFilterQuery(ids), nil, &result); err != nil {
		return nil, err
	}
	if err := rejectIncompletePage(len(result.Embedded.Tasks), enrichmentBatchLimit, result.Links.Next.Href); err != nil {
		return nil, err
	}
	want := requestedIDs(ids)
	tasks := make([]Task, 0, len(result.Embedded.Tasks))
	for _, item := range result.Embedded.Tasks {
		if _, ok := want[item.ID]; !ok {
			return nil, ErrIncompleteResponse
		}
		text, err := taskResultText(item.Result)
		if err != nil || len(item.Text) > enrichmentPayloadLimit || len(text) > enrichmentPayloadLimit {
			tasks = append(tasks, Task{ID: item.ID, Invalid: true})
			continue
		}
		tasks = append(tasks, Task{
			ID: item.ID, EntityID: item.EntityID, ResponsibleUserID: item.ResponsibleUserID,
			CompleteTill: item.CompleteTill, TaskTypeID: item.TaskTypeID, UpdatedAt: item.UpdatedAt,
			EntityType: item.EntityType, Text: item.Text, ResultText: text, IsCompleted: item.IsCompleted,
		})
	}
	return tasks, nil
}

func taskResultText(raw json.RawMessage) (string, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) || bytes.HasPrefix(value, []byte("[")) {
		return "", nil
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(value, &result); err != nil {
		return "", ErrIncompleteResponse
	}
	return result.Text, nil
}

func (c *Client) ListPipelines(ctx context.Context, installationID uuid.UUID) ([]Pipeline, error) {
	var result struct {
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
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/leads/pipelines", nil, &result); err != nil {
		return nil, err
	}
	if err := rejectIncompletePage(len(result.Embedded.Pipelines), enrichmentPipelineLimit, result.Links.Next.Href); err != nil {
		return nil, err
	}
	pipelines := make([]Pipeline, 0, len(result.Embedded.Pipelines))
	for _, item := range result.Embedded.Pipelines {
		if item.ID <= 0 || len(item.Name) > enrichmentNameLimit || len(item.Embedded.Statuses) > enrichmentStatusLimit {
			return nil, ErrIncompleteResponse
		}
		statuses := make([]PipelineStatus, 0, len(item.Embedded.Statuses))
		for _, status := range item.Embedded.Statuses {
			if status.ID <= 0 || len(status.Name) > enrichmentNameLimit {
				return nil, ErrIncompleteResponse
			}
			statuses = append(statuses, PipelineStatus{ID: status.ID, Name: status.Name})
		}
		pipelines = append(pipelines, Pipeline{ID: item.ID, Name: item.Name, Statuses: statuses})
	}
	return pipelines, nil
}

func (c *Client) ListCustomFields(ctx context.Context, installationID uuid.UUID, entityType string) ([]CustomField, error) {
	plural, err := catalogEntityType(entityType)
	if err != nil {
		return nil, err
	}
	fields := []CustomField{}
	for page := 1; page <= enrichmentFieldPages; page++ {
		q := url.Values{"limit": {strconv.Itoa(enrichmentFieldPageSize)}, "page": {strconv.Itoa(page)}}
		var result struct {
			Embedded struct {
				CustomFields []struct {
					ID    int64             `json:"id"`
					Name  string            `json:"name"`
					Type  string            `json:"type"`
					Enums []CustomFieldEnum `json:"enums"`
				} `json:"custom_fields"`
			} `json:"_embedded"`
			Links struct {
				Next struct {
					Href string `json:"href"`
				} `json:"next"`
			} `json:"_links"`
		}
		if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/"+plural+"/custom_fields?"+q.Encode(), nil, &result); err != nil {
			return nil, err
		}
		if err := rejectIncompletePage(len(result.Embedded.CustomFields), enrichmentFieldPageSize, result.Links.Next.Href); err != nil {
			return nil, err
		}
		if len(fields)+len(result.Embedded.CustomFields) > enrichmentFieldLimit {
			return nil, ErrIncompleteResponse
		}
		for _, item := range result.Embedded.CustomFields {
			if item.ID <= 0 || len(item.Name) > enrichmentNameLimit {
				return nil, ErrIncompleteResponse
			}
			enums := item.Enums
			if enums == nil {
				enums = []CustomFieldEnum{}
			}
			fields = append(fields, CustomField{
				ID: item.ID, Name: item.Name, Type: item.Type, EntityType: plural, Enums: enums,
			})
		}
		if result.Links.Next.Href == "" {
			return fields, nil
		}
	}
	return nil, ErrIncompleteResponse
}

func (c *Client) ListEntities(ctx context.Context, installationID uuid.UUID, entityType string, ids []int64) ([]EntityName, error) {
	plural, err := catalogEntityType(entityType)
	if err != nil {
		return nil, err
	}
	if err := validateIDBatch(ids); err != nil {
		return nil, err
	}
	var result struct {
		Embedded map[string][]struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"_embedded"`
		Links struct {
			Next struct {
				Href string `json:"href"`
			} `json:"next"`
		} `json:"_links"`
	}
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/"+plural+"?"+idFilterQuery(ids), nil, &result); err != nil {
		return nil, err
	}
	items := result.Embedded[plural]
	if err := rejectIncompletePage(len(items), enrichmentBatchLimit, result.Links.Next.Href); err != nil {
		return nil, err
	}
	want := requestedIDs(ids)
	entities := make([]EntityName, 0, len(items))
	for _, item := range items {
		if _, ok := want[item.ID]; !ok {
			return nil, ErrIncompleteResponse
		}
		if len(item.Name) > enrichmentNameLimit {
			entities = append(entities, EntityName{ID: item.ID, Invalid: true})
			continue
		}
		entities = append(entities, EntityName{ID: item.ID, EntityType: plural, Name: item.Name})
	}
	return entities, nil
}
