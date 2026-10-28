package amocrm

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

const (
	openTaskPageSize   = 250
	taskTypeLimit      = 500
	participantLimit   = 20
	participantLeadCap = 250
)

// OpenTaskPage is one page of a responsible user's unfinished tasks, ordered
// by deadline. HasNext reports amoCRM's next link, never a guess.
type OpenTaskPage struct {
	Tasks   []Task
	HasNext bool
}

type TaskType struct {
	ID   int64
	Name string
}

// EntityContext is the bounded card summary that decorates an activity
// session. Pipeline and status are leads-only and zero elsewhere.
type EntityContext struct {
	ID, ResponsibleUserID, PipelineID, StatusID int64
	EntityType, Name                            string
}

type openTasksWire struct {
	empty    bool
	Embedded struct {
		Tasks []taskWire `json:"tasks"`
	} `json:"_embedded"`
	Links struct {
		Next struct {
			Href string `json:"href"`
		} `json:"next"`
	} `json:"_links"`
}

func (w *openTasksWire) NoContent() { w.empty = true }

type taskWire struct {
	ID                int64  `json:"id"`
	EntityID          int64  `json:"entity_id"`
	ResponsibleUserID int64  `json:"responsible_user_id"`
	CompleteTill      int64  `json:"complete_till"`
	TaskTypeID        int64  `json:"task_type_id"`
	UpdatedAt         int64  `json:"updated_at"`
	EntityType        string `json:"entity_type"`
	Text              string `json:"text"`
	IsCompleted       bool   `json:"is_completed"`
}

// ListOpenTasksPage reads unfinished tasks of exactly one responsible user. A
// row for another user or a completed task means the filter was not applied.
func (c *Client) ListOpenTasksPage(ctx context.Context, installationID uuid.UUID, responsibleUserID int64, page int) (OpenTaskPage, error) {
	if responsibleUserID <= 0 || page < 1 {
		return OpenTaskPage{}, fmt.Errorf("invalid open task page request")
	}
	q := url.Values{
		"limit":                       {strconv.Itoa(openTaskPageSize)},
		"page":                        {strconv.Itoa(page)},
		"filter[responsible_user_id]": {strconv.FormatInt(responsibleUserID, 10)},
		"filter[is_completed]":        {"0"},
		"order[complete_till]":        {"asc"},
	}
	var result openTasksWire
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/tasks?"+q.Encode(), nil, &result); err != nil {
		return OpenTaskPage{}, err
	}
	if result.empty {
		return OpenTaskPage{Tasks: []Task{}}, nil
	}
	if err := rejectIncompletePage(len(result.Embedded.Tasks), openTaskPageSize, result.Links.Next.Href); err != nil {
		return OpenTaskPage{}, err
	}
	tasks := make([]Task, 0, len(result.Embedded.Tasks))
	for _, item := range result.Embedded.Tasks {
		if item.ID <= 0 || item.ResponsibleUserID != responsibleUserID || item.IsCompleted {
			return OpenTaskPage{}, ErrIncompleteResponse
		}
		task := Task{
			ID: item.ID, EntityID: item.EntityID, ResponsibleUserID: item.ResponsibleUserID,
			CompleteTill: item.CompleteTill, TaskTypeID: item.TaskTypeID, UpdatedAt: item.UpdatedAt,
			EntityType: item.EntityType, Text: item.Text,
		}
		// The task is still the employee's work; only its oversized text is dropped.
		if len(item.Text) > enrichmentPayloadLimit {
			task.Text, task.Invalid = "", true
		}
		tasks = append(tasks, task)
	}
	return OpenTaskPage{Tasks: tasks, HasNext: result.Links.Next.Href != ""}, nil
}

// HasOpenTask reports live whether the user is responsible for an unfinished
// task on the card. A row outside the filter means it was not applied.
func (c *Client) HasOpenTask(ctx context.Context, installationID uuid.UUID, responsibleUserID int64, entityType string, entityID int64) (bool, error) {
	plural, err := catalogEntityType(entityType)
	if err != nil {
		return false, err
	}
	if responsibleUserID <= 0 || entityID <= 0 {
		return false, fmt.Errorf("invalid open task check")
	}
	q := url.Values{
		"limit":                       {"1"},
		"filter[responsible_user_id]": {strconv.FormatInt(responsibleUserID, 10)},
		"filter[is_completed]":        {"0"},
		"filter[entity_type]":         {plural},
		"filter[entity_id][]":         {strconv.FormatInt(entityID, 10)},
	}
	var result openTasksWire
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/tasks?"+q.Encode(), nil, &result); err != nil {
		return false, err
	}
	if result.empty || len(result.Embedded.Tasks) == 0 {
		return false, nil
	}
	if len(result.Embedded.Tasks) > 1 {
		return false, ErrIncompleteResponse
	}
	item := result.Embedded.Tasks[0]
	if item.ID <= 0 || item.ResponsibleUserID != responsibleUserID || item.EntityType != plural || item.EntityID != entityID || item.IsCompleted {
		return false, ErrIncompleteResponse
	}
	return true, nil
}

// CompleteTask applies amoCRM's own completion with a result. Callers must
// read the task first: a lost response is resolved by reading, not by retry.
func (c *Client) CompleteTask(ctx context.Context, installationID uuid.UUID, taskID int64, result string) error {
	if taskID <= 0 {
		return fmt.Errorf("amoCRM task id must be positive")
	}
	body := map[string]any{"is_completed": true, "result": map[string]string{"text": result}}
	var response struct {
		ID int64 `json:"id"`
	}
	if err := c.DoJSON(ctx, installationID, http.MethodPatch, fmt.Sprintf("/api/v4/tasks/%d", taskID), body, &response); err != nil {
		return err
	}
	if response.ID != taskID {
		return ErrIncompleteResponse
	}
	return nil
}

func (c *Client) ListTaskTypes(ctx context.Context, installationID uuid.UUID) ([]TaskType, error) {
	var account struct {
		Embedded struct {
			TaskTypes []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"task_types"`
		} `json:"_embedded"`
	}
	if err := c.DoJSON(ctx, installationID, http.MethodGet, "/api/v4/account?with=task_types", nil, &account); err != nil {
		return nil, err
	}
	if len(account.Embedded.TaskTypes) > taskTypeLimit {
		return nil, ErrIncompleteResponse
	}
	types := make([]TaskType, 0, len(account.Embedded.TaskTypes))
	for _, item := range account.Embedded.TaskTypes {
		if item.ID <= 0 || len(item.Name) > enrichmentNameLimit {
			return nil, ErrIncompleteResponse
		}
		types = append(types, TaskType{ID: item.ID, Name: item.Name})
	}
	return types, nil
}

type linkWire struct {
	ID int64 `json:"id"`
}

type entityWire struct {
	absent            bool
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	ResponsibleUserID int64  `json:"responsible_user_id"`
	PipelineID        int64  `json:"pipeline_id"`
	StatusID          int64  `json:"status_id"`
	Embedded          struct {
		Contacts  []linkWire `json:"contacts"`
		Companies []linkWire `json:"companies"`
		Leads     []linkWire `json:"leads"`
	} `json:"_embedded"`
}

func (w *entityWire) NoContent() { w.absent = true }

func (c *Client) readEntity(ctx context.Context, installationID uuid.UUID, entityType string, id int64, with string) (entityWire, string, error) {
	plural, err := catalogEntityType(entityType)
	if err != nil {
		return entityWire{}, "", err
	}
	if id <= 0 {
		return entityWire{}, "", fmt.Errorf("amoCRM entity id must be positive")
	}
	path := fmt.Sprintf("/api/v4/%s/%d", plural, id)
	if with != "" {
		path += "?with=" + with
	}
	var item entityWire
	if err := c.DoJSON(ctx, installationID, http.MethodGet, path, nil, &item); err != nil {
		return entityWire{}, "", err
	}
	// amoCRM answers 204 for a deleted or unknown card.
	if item.absent {
		return entityWire{}, "", &APIError{Kind: ErrorNotFound, StatusCode: http.StatusNoContent}
	}
	if item.ID != id || len(item.Name) > enrichmentNameLimit {
		return entityWire{}, "", ErrIncompleteResponse
	}
	return item, plural, nil
}

func (c *Client) GetEntityContext(ctx context.Context, installationID uuid.UUID, entityType string, id int64) (EntityContext, error) {
	item, plural, err := c.readEntity(ctx, installationID, entityType, id, "")
	if err != nil {
		return EntityContext{}, err
	}
	result := EntityContext{ID: item.ID, EntityType: plural, Name: item.Name, ResponsibleUserID: item.ResponsibleUserID}
	if plural == "leads" {
		result.PipelineID, result.StatusID = item.PipelineID, item.StatusID
	}
	return result, nil
}

func linkIDs(links []linkWire, limit int) []int64 {
	ids := make([]int64, 0, min(len(links), limit))
	for _, ref := range links {
		if ref.ID > 0 && len(ids) < limit {
			ids = append(ids, ref.ID)
		}
	}
	return ids
}

// LeadParticipants lists contacts and companies linked to a lead. Links only
// decorate a queue, so an unusually large card is capped, not rejected.
func (c *Client) LeadParticipants(ctx context.Context, installationID uuid.UUID, leadID int64) (contacts, companies []int64, err error) {
	item, _, err := c.readEntity(ctx, installationID, "leads", leadID, "contacts")
	if err != nil {
		return nil, nil, err
	}
	return linkIDs(item.Embedded.Contacts, participantLimit), linkIDs(item.Embedded.Companies, participantLimit), nil
}

// ParticipantLeads returns a contact's or company's name and linked leads.
func (c *Client) ParticipantLeads(ctx context.Context, installationID uuid.UUID, entityType string, id int64) (string, []int64, error) {
	if entityType != "contacts" && entityType != "companies" {
		return "", nil, fmt.Errorf("participants are contacts or companies")
	}
	item, _, err := c.readEntity(ctx, installationID, entityType, id, "leads")
	if err != nil {
		return "", nil, err
	}
	return item.Name, linkIDs(item.Embedded.Leads, participantLeadCap), nil
}
