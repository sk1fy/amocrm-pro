package admincommand

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
)

func TestImmediateConflictRetainsEnrichedAndWrappedErrors(t *testing.T) {
	enriched := conflictDetails("settings were updated", map[string]any{"updated_at": int64(42)})
	for _, command := range []string{"activity-configure", "activity-panel-patch", "lead-status-configure"} {
		t.Run(command, func(t *testing.T) {
			for _, err := range []error{
				enriched,
				fmt.Errorf("read current state: %w", enriched),
				serviceapi.Fail(serviceapi.Conflict, "revision changed"),
				leadstatus.ErrRuleRevisionConflict,
			} {
				if !immediateConflict(command, err) {
					t.Errorf("conflict not recognized: %v", err)
				}
			}
			for _, err := range []error{nil, invalid("invalid input"), errors.New("unavailable")} {
				if immediateConflict(command, err) {
					t.Errorf("non-conflict recognized: %v", err)
				}
			}
		})
	}
	if immediateConflict("disable", enriched) {
		t.Fatal("non-CAS command must retain its durable failed receipt")
	}
	if got := classify(fmt.Errorf("wrapped: %w", enriched)); got != enriched || got.Details["updated_at"] != int64(42) {
		t.Fatal("current state details were lost")
	}
}
