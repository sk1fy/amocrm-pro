package serviceapi

import (
	"reflect"
	"testing"
)

func TestUserGrantsForLeastPrivilege(t *testing.T) {
	cases := []struct {
		audience, action string
		want             []Grant
	}{
		{ActivityService, ActionPanel, []Grant{{ActivityService, ActionPanel}, {EventsService, ActionRead}, {GatewayService, ActionUsers}}},
		{ActivityService, ActionSettings, []Grant{{ActivityService, ActionSettings}}},
		{ActivityService, ActionOperation, []Grant{{ActivityService, ActionOperation}}},
		{EventsService, ActionRead, []Grant{{EventsService, ActionRead}}},
		{EventsService, ActionStatus, []Grant{{EventsService, ActionStatus}}},
		{EventsService, ActionSync, []Grant{{EventsService, ActionSync}}},
		{EventsService, ActionOperation, []Grant{{EventsService, ActionOperation}}},
		{GatewayService, ActionEvents, nil}, {ActivityService, ActionSync, nil}, {"", ActionPanel, nil}, {ActivityService, "future", nil},
	}
	for _, c := range cases {
		t.Run(c.audience+"/"+c.action, func(t *testing.T) {
			if got := UserGrantsFor(c.audience, c.action); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got%+v want%+v", got, c.want)
			}
		})
	}
}
