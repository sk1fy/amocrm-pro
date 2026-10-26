package serviceapi

import (
	"encoding/json"
	"github.com/google/uuid"
	"slices"
)

const ActionSessions = "sessions"
const PrincipalKindEmployee = "employee"

type SessionContext struct {
	ClientID     string `json:"client_id"`
	TaskID       int64  `json:"task_id"`
	TaskName     string `json:"task_name"`
	TaskTypeID   int64  `json:"task_type_id"`
	TaskTypeName string `json:"task_type_name"`
	TaskResult   string `json:"task_result"`
	ActivityName string `json:"activity_name"`
	ActivityType string `json:"activity_type"`
	EntityID     int64  `json:"entity_id"`
	EntityType   string `json:"entity_type"`
	EntityName   string `json:"entity_name"`
	PipelineID   int64  `json:"pipeline_id"`
	PipelineName string `json:"pipeline_name"`
	StatusID     int64  `json:"status_id"`
	StatusName   string `json:"status_name"`
	StatusColor  string `json:"status_color"`
}
type WorkSession struct {
	ID              string         `json:"id"`
	EmployeeID      int64          `json:"employee_id"`
	Kind            string         `json:"kind"`
	StartedAt       int64          `json:"started_at"`
	StoppedAt       int64          `json:"stopped_at"`
	WorkTimeSeconds int64          `json:"work_time_seconds"`
	Context         SessionContext `json:"context"`
}
type SessionCommand struct {
	ExpectedSessionID  string         `json:"expected_session_id"`
	HasExpectedSession bool           `json:"has_expected_session"`
	Auth               Auth           `json:"auth"`
	CommandID          string         `json:"command_id"`
	Action             string         `json:"action"`
	SessionID          string         `json:"session_id"`
	Context            SessionContext `json:"context"`
}
type SessionPage struct {
	NextCursor string        `json:"next_cursor,omitempty"`
	Items      []WorkSession `json:"items"`
	SnapshotAt int64         `json:"snapshot_at"`
}
type TaskPanelSettings struct {
	Enabled             bool     `json:"enabled"`
	EmployeeIDs         []int64  `json:"employee_ids"`
	ConsiderFutureTasks bool     `json:"consider_future_tasks"`
	IncludeRelatedDeals bool     `json:"include_related_deals"`
	PriorityKeys        []string `json:"priority_keys"`
	Revision            int64    `json:"revision"`
}
type TaskPanelCommand struct {
	Auth     Auth              `json:"auth"`
	Settings TaskPanelSettings `json:"settings"`
}

func ValidateSessionCommand(c SessionCommand) error {
	if id, e := uuid.Parse(c.CommandID); e != nil || id == uuid.Nil {
		return Fail(InvalidArgument, "command_id must be a UUID")
	}
	if c.Action != "work" && c.Action != "activity" && c.Action != "pause" && c.Action != "stop" {
		return Fail(InvalidArgument, "invalid session action")
	}
	if c.HasExpectedSession && c.ExpectedSessionID != "" {
		if id, e := uuid.Parse(c.ExpectedSessionID); e != nil || id == uuid.Nil {
			return Fail(InvalidArgument, "invalid expected_session_id")
		}
	}
	if c.Context.ClientID != "" {
		if id, e := uuid.Parse(c.Context.ClientID); e != nil || id == uuid.Nil {
			return Fail(InvalidArgument, "invalid client_id")
		}
	}
	if c.Action == "stop" {
		if id, e := uuid.Parse(c.SessionID); e != nil || id == uuid.Nil {
			return Fail(InvalidArgument, "stop requires session_id")
		}
	}
	if c.Action == "work" && c.Context.TaskID <= 0 {
		return Fail(InvalidArgument, "work session requires task_id")
	}
	if c.Action == "activity" && c.Context.ActivityName == "" {
		return Fail(InvalidArgument, "activity requires a name")
	}
	if c.Context.TaskID < 0 || c.Context.EntityID < 0 || c.Context.PipelineID < 0 || c.Context.StatusID < 0 || c.Context.TaskTypeID < 0 {
		return Fail(InvalidArgument, "negative context ID")
	}
	if c.Context.EntityType != "" && !slices.Contains([]string{"leads", "contacts", "companies", "customers"}, c.Context.EntityType) {
		return Fail(InvalidArgument, "invalid entity type")
	}
	for _, s := range []string{c.Context.TaskName, c.Context.TaskResult, c.Context.ActivityName, c.Context.EntityName, c.Context.PipelineName, c.Context.StatusName, c.Context.TaskTypeName} {
		if len(s) > 2000 {
			return Fail(InvalidArgument, "session context text too long")
		}
	}
	if len(c.Context.ActivityType) > 32 || len(c.Context.StatusColor) > 16 {
		return Fail(InvalidArgument, "session context field too long")
	}
	data, _ := json.Marshal(c.Context)
	if len(data) > 16384 {
		return Fail(InvalidArgument, "session context exceeds limit")
	}
	return nil
}
func ValidateTaskPanel(s TaskPanelSettings) error {
	if s.Revision < 0 || len(s.EmployeeIDs) > 1000 || len(s.PriorityKeys) > 6 {
		return Fail(InvalidArgument, "invalid task panel settings")
	}
	seen := map[int64]bool{}
	for _, id := range s.EmployeeIDs {
		if id <= 0 || seen[id] {
			return Fail(InvalidArgument, "invalid employee IDs")
		}
		seen[id] = true
	}
	keys := map[string]bool{}
	for _, k := range s.PriorityKeys {
		if keys[k] || !slices.Contains([]string{"urgent", "overdue", "overdue_no_time", "today_no_time", "today_timed", "future"}, k) {
			return Fail(InvalidArgument, "invalid task priority")
		}
		keys[k] = true
	}
	return nil
}

type EmployeeActivityState struct {
	Current  WorkSession       `json:"current"`
	Settings TaskPanelSettings `json:"settings"`
	Enabled  bool              `json:"enabled"`
}
