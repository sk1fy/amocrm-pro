package serviceapi

import "context"

type ProblemReader interface {
	Problems(context.Context, ProblemRequest) (ProblemSnapshot, error)
}

const ActionProblems = "problems"

type ProblemRequest struct {
	Auth        Auth    `json:"auth"`
	EmployeeIDs []int64 `json:"employee_ids"`
	PipelineIDs []int64 `json:"pipeline_ids"`
}
type ProblemCount struct {
	EmployeeID        int64 `json:"employee_id"`
	LeadsNoTasks      int   `json:"leads_no_tasks"`
	LeadsOverdueTasks int   `json:"leads_overdue_tasks"`
	OverdueTasks      int   `json:"overdue_tasks"`
}
type ProblemSnapshot struct {
	Stale      bool           `json:"stale"`
	State      string         `json:"state"`
	ErrorCode  string         `json:"error_code,omitempty"`
	Counts     []ProblemCount `json:"counts"`
	SnapshotAt int64          `json:"snapshot_at"`
}
