package api

import (
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"slices"
	"testing"
)

func TestActivityOpenAPIOperationStatesMatchApplication(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{serviceapi.OperationAccepted, serviceapi.OperationRunning, serviceapi.OperationRetry, serviceapi.OperationPaused, serviceapi.OperationFailed, serviceapi.OperationSucceeded}
	for _, name := range []string{"ActivityOwnerOperation", "ActivityReceipt"} {
		expected := slices.Clone(want)
		if name == "ActivityReceipt" {
			expected = append(expected, "pending_delivery")
		}
		slices.Sort(expected)
		var actual []string
		for _, value := range document.Components.Schemas[name].Value.Properties["state"].Value.Enum {
			actual = append(actual, value.(string))
		}
		slices.Sort(actual)
		if !slices.Equal(expected, actual) {
			t.Fatalf("%s states=%v want=%v", name, actual, expected)
		}
	}
	// Existing Core widget job status is a separate public contract.
	coreJobStates := document.Components.Schemas["JobStatus"].Value.Properties["status"].Value.Enum
	if !slices.Contains(coreJobStates, any("completed")) {
		t.Fatal("existing Core job completed status was changed")
	}
}
