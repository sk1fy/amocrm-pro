package serviceapi

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Gateway actions for an employee's own amoCRM tasks. Only the employee
// principal receives them, and Gateway enforces responsible_user_id itself.
const (
	ActionOwnTasks     = "own_tasks"
	ActionCompleteTask = "complete_task"
)

const TaskResultLimit = 2000

// EmployeeTasks is implemented by Activity (scenario) and Gateway (amoCRM
// access). It is optional so older peers keep their existing contracts.
type EmployeeTasks interface {
	OwnTasks(context.Context, OwnTasksRequest) (TaskQueue, error)
	OwnTask(context.Context, OwnTaskRequest) (TaskDetail, error)
	CompleteOwnTask(context.Context, CompleteTaskRequest) (TaskDetail, error)
	OwnLead(context.Context, OwnLeadRequest) (EntityContext, error)
}

// LeadID is the card currently open in the browser; Gateway uses it only to
// label tasks of the queue that are related to that card.
type OwnTasksRequest struct {
	Auth   Auth  `json:"auth"`
	LeadID int64 `json:"lead_id"`
}
type QueueTask struct {
	ID           int64  `json:"id"`
	EntityID     int64  `json:"entity_id"`
	EntityType   string `json:"entity_type"`
	EntityName   string `json:"entity_name"`
	Text         string `json:"text"`
	CompleteTill int64  `json:"complete_till"`
	TaskTypeID   int64  `json:"task_type_id"`
	TaskTypeName string `json:"task_type_name"`
	Related      string `json:"related"`
	UpdatedAt    int64  `json:"updated_at"`
}

// TaskQueue is a shared snapshot of unfinished tasks. Complete is false when
// the page cap truncated the queue; Stale marks data past FreshUntil that is
// served while amoCRM is busy or a refresh is in progress.
type TaskQueue struct {
	Tasks             []QueueTask `json:"tasks"`
	FetchedAt         int64       `json:"fetched_at"`
	FreshUntil        int64       `json:"fresh_until"`
	Complete          bool        `json:"complete"`
	Stale             bool        `json:"stale"`
	ErrorCode         string      `json:"error_code,omitempty"`
	RetryAfterSeconds int64       `json:"retry_after_seconds,omitempty"`
}

type OwnTaskRequest struct {
	Auth        Auth  `json:"auth"`
	TaskID      int64 `json:"task_id"`
	WithContext bool  `json:"with_context"`
}
type TaskDetail struct {
	ID                int64         `json:"id"`
	EntityID          int64         `json:"entity_id"`
	EntityType        string        `json:"entity_type"`
	ResponsibleUserID int64         `json:"responsible_user_id"`
	Text              string        `json:"text"`
	CompleteTill      int64         `json:"complete_till"`
	TaskTypeID        int64         `json:"task_type_id"`
	TaskTypeName      string        `json:"task_type_name"`
	IsCompleted       bool          `json:"is_completed"`
	ResultText        string        `json:"result_text"`
	UpdatedAt         int64         `json:"updated_at"`
	Entity            EntityContext `json:"entity"`
}
type CompleteTaskRequest struct {
	Auth   Auth   `json:"auth"`
	TaskID int64  `json:"task_id"`
	Result string `json:"result"`
}
type OwnLeadRequest struct {
	Auth   Auth  `json:"auth"`
	LeadID int64 `json:"lead_id"`
}

// EntityContext names a card. Restricted means the server account could read
// it, but nothing proves the employee may see it, so only the ID is returned.
type EntityContext struct {
	ID           int64  `json:"id"`
	EntityType   string `json:"entity_type"`
	Name         string `json:"name"`
	PipelineID   int64  `json:"pipeline_id"`
	PipelineName string `json:"pipeline_name"`
	StatusID     int64  `json:"status_id"`
	StatusName   string `json:"status_name"`
	StatusColor  string `json:"status_color"`
	Restricted   bool   `json:"restricted"`
}

func ValidateTaskID(id int64) error {
	if id <= 0 {
		return Fail(InvalidArgument, "task_id must be positive")
	}
	return nil
}

func ValidateTaskResult(result string) error {
	if !utf8.ValidString(result) || len(result) > TaskResultLimit {
		return Fail(InvalidArgument, "result must be valid UTF-8 of at most 2000 bytes")
	}
	if strings.TrimSpace(result) == "" {
		return Fail(InvalidArgument, "result is required to complete a task")
	}
	return nil
}
