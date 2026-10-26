package serviceapi

import "slices"

type CallThresholds struct {
	DialingSeconds      int `json:"dialing_seconds"`
	ConversationSeconds int `json:"conversation_seconds"`
	ExceedingSeconds    int `json:"exceeding_seconds"`
}
type OperatorSettings struct {
	EmployeeID int64          `json:"employee_id"`
	Enabled    bool           `json:"enabled"`
	Plan       int            `json:"plan"`
	Thresholds CallThresholds `json:"thresholds"`
}
type DepartmentThresholds struct {
	GroupID    int64          `json:"group_id"`
	Thresholds CallThresholds `json:"thresholds"`
}
type PanelOptions struct {
	PipelineIDs []int64                `json:"pipeline_ids"`
	Operators   []OperatorSettings     `json:"operators"`
	Departments []DepartmentThresholds `json:"departments"`
}

func ValidatePanelOptions(o PanelOptions, employees []int64) error {
	if len(o.PipelineIDs) > 100 || len(o.Operators) > 100 || len(o.Departments) > 100 {
		return Fail(InvalidArgument, "at most 100 pipelines, operators and departments")
	}
	seen := map[int64]bool{}
	for _, id := range o.PipelineIDs {
		if id <= 0 || seen[id] {
			return Fail(InvalidArgument, "pipeline ids must be positive and unique")
		}
		seen[id] = true
	}
	seen = map[int64]bool{}
	for _, v := range o.Operators {
		if v.EmployeeID <= 0 || seen[v.EmployeeID] || !slices.Contains(employees, v.EmployeeID) || v.Plan < 1 || v.Plan > 100000 || !validThresholds(v.Thresholds) {
			return Fail(InvalidArgument, "invalid operator, plan or thresholds")
		}
		seen[v.EmployeeID] = true
	}
	seen = map[int64]bool{}
	for _, v := range o.Departments {
		if v.GroupID < 0 || seen[v.GroupID] || !validThresholds(v.Thresholds) {
			return Fail(InvalidArgument, "invalid department thresholds")
		}
		seen[v.GroupID] = true
	}
	return nil
}
func validThresholds(t CallThresholds) bool {
	return t.DialingSeconds >= 0 && t.DialingSeconds < t.ConversationSeconds && t.ConversationSeconds < t.ExceedingSeconds && t.ExceedingSeconds <= 86400
}
