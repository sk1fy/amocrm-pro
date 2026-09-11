package serviceapi

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	MaxPanelsPerInstallation = 50
	PanelNameMaxLen          = 120
	ViewKeyBytes             = 32
)

type DisplayWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ShareLookupRequest struct {
	ViewKey string `json:"view_key"`
}

type ShareLookup struct {
	Scope
	PanelID        uuid.UUID `json:"panel_id"`
	Enabled        bool      `json:"enabled"`
	ViewKeyVersion int       `json:"view_key_version"`
	EmployeeIDs    []int64   `json:"employee_ids"`
}

type PanelRef struct {
	Auth    Auth      `json:"auth"`
	PanelID uuid.UUID `json:"panel_id"`
}

type PanelCommand struct {
	Auth          Auth          `json:"auth"`
	CommandID     string        `json:"command_id"`
	PanelID       uuid.UUID     `json:"panel_id,omitempty"`
	Name          string        `json:"name,omitempty"`
	EmployeeIDs   []int64       `json:"employee_ids,omitempty"`
	DisplayWindow DisplayWindow `json:"display_window"`
	Enabled       *bool         `json:"enabled,omitempty"`
	Revision      int64         `json:"revision,omitempty"`
	HasName       bool          `json:"-"`
	HasEmployees  bool          `json:"-"`
	HasWindow     bool          `json:"-"`
	Rotate        bool          `json:"-"`
}

type ManagedPanel struct {
	ID             uuid.UUID     `json:"id"`
	Name           string        `json:"name"`
	EmployeeIDs    []int64       `json:"employee_ids"`
	DisplayWindow  DisplayWindow `json:"display_window"`
	Timezone       string        `json:"timezone"`
	Enabled        bool          `json:"enabled"`
	Revision       int64         `json:"revision"`
	UpdatedAt      time.Time     `json:"updated_at"`
	ViewKeyVersion int           `json:"-"`
	ShareUrlIssued bool          `json:"share_url_issued"`
	ViewKey        string        `json:"view_key,omitempty"`
	ShareUrl       string        `json:"share_url,omitempty"`
}

type ManagedPanelPage struct {
	Panels []ManagedPanel `json:"panels"`
}

type ViewerPanel struct {
	Name                  string        `json:"name"`
	Timezone              string        `json:"timezone"`
	DisplayWindow         DisplayWindow `json:"display_window"`
	Employees             []User        `json:"employees"`
	InterpretationVersion int           `json:"interpretation_version,omitempty"`
}

type ViewerTimeline struct {
	Employees             []User      `json:"employees"`
	Timezone              string      `json:"timezone"`
	Data                  QueryResult `json:"data"`
	Coverage              string      `json:"coverage,omitempty"`
	Freshness             string      `json:"freshness,omitempty"`
	EmptyReason           string      `json:"empty_reason,omitempty"`
	InterpretationVersion int         `json:"interpretation_version,omitempty"`
}

type ViewerEmployee struct {
	Employee              User        `json:"employee"`
	Timezone              string      `json:"timezone"`
	Data                  QueryResult `json:"data"`
	Coverage              string      `json:"coverage,omitempty"`
	Freshness             string      `json:"freshness,omitempty"`
	EmptyReason           string      `json:"empty_reason,omitempty"`
	InterpretationVersion int         `json:"interpretation_version,omitempty"`
}

func ValidateDisplayWindow(w DisplayWindow) error {
	if !validClock(w.From) || !validClock(w.To) || w.From == w.To {
		return Fail(InvalidArgument, "display_window.from and display_window.to must be distinct HH:MM values")
	}
	return nil
}

func validClock(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	hour := int(value[0]-'0')*10 + int(value[1]-'0')
	minute := int(value[3]-'0')*10 + int(value[4]-'0')
	if value[0] < '0' || value[0] > '9' || value[1] < '0' || value[1] > '9' || value[3] < '0' || value[3] > '9' || value[4] < '0' || value[4] > '9' {
		return false
	}
	return hour <= 23 && minute <= 59
}

func ValidatePanelName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed != name || trimmed == "" || len(trimmed) > PanelNameMaxLen {
		return Fail(InvalidArgument, "panel name must be 1..120 characters without surrounding whitespace")
	}
	return nil
}

func ValidateEmployeeIDs(ids []int64) error {
	if len(ids) == 0 || len(ids) > 100 {
		return Fail(InvalidArgument, "select between 1 and 100 employees")
	}
	seen := map[int64]struct{}{}
	for _, id := range ids {
		if id <= 0 {
			return Fail(InvalidArgument, "employee id must be positive")
		}
		if _, ok := seen[id]; ok {
			return Fail(InvalidArgument, "employee ids must be unique")
		}
		seen[id] = struct{}{}
	}
	return nil
}
