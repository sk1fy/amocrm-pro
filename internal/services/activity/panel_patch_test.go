package activity

import (
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestPanelPatchRequestHashIncludesEveryMeaningfulFieldWithoutInt64Loss(t *testing.T) {
	disabled := false
	principal := serviceapi.Principal{
		Scope:   serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()},
		ActorID: math.MaxInt64,
	}
	command := serviceapi.PanelCommand{
		PanelID: uuid.New(), Revision: math.MaxInt64,
		Name: "Night shift", HasName: true,
		EmployeeIDs: []int64{1<<53 + 1, math.MaxInt64}, HasEmployees: true,
		DisplayWindow: serviceapi.DisplayWindow{From: "22:00", To: "06:00"}, HasWindow: true,
		Enabled: &disabled,
	}
	want := panelPatchRequestHash(principal, command)
	assertDifferent := func(name string, p serviceapi.Principal, c serviceapi.PanelCommand) {
		t.Helper()
		if got := panelPatchRequestHash(p, c); got == want {
			t.Fatalf("%s was omitted from request hash", name)
		}
	}

	p := principal
	p.InstallationID = uuid.New()
	assertDifferent("installation", p, command)
	p = principal
	p.IntegrationID = uuid.New()
	assertDifferent("integration", p, command)
	p = principal
	p.ActorID--
	assertDifferent("actor", p, command)

	c := command
	c.PanelID = uuid.New()
	assertDifferent("panel id", principal, c)
	c = command
	c.Revision--
	assertDifferent("revision", principal, c)
	c = command
	c.Name = "Day shift"
	assertDifferent("name value", principal, c)
	c = command
	c.HasName = false
	assertDifferent("name presence", principal, c)
	c = command
	c.EmployeeIDs = []int64{1 << 53, math.MaxInt64}
	assertDifferent("employee above 2^53", principal, c)
	c = command
	c.EmployeeIDs = []int64{1<<53 + 1, math.MaxInt64 - 1}
	assertDifferent("MaxInt64 employee", principal, c)
	c = command
	c.HasEmployees = false
	assertDifferent("employees presence", principal, c)
	c = command
	c.DisplayWindow.From = "21:00"
	assertDifferent("window from", principal, c)
	c = command
	c.DisplayWindow.To = "07:00"
	assertDifferent("window to", principal, c)
	c = command
	c.HasWindow = false
	assertDifferent("window presence", principal, c)
	c = command
	c.Enabled = nil
	assertDifferent("enabled absence", principal, c)
	enabled := true
	c = command
	c.Enabled = &enabled
	assertDifferent("enabled value", principal, c)
}
