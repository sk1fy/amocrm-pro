package serviceapi

import "testing"

func TestPanelOptionsRejectForeignOperatorsAndInvalidThresholds(t *testing.T) {
	good := PanelOptions{Operators: []OperatorSettings{{EmployeeID: 7, Plan: 100, Thresholds: CallThresholds{DialingSeconds: 10, ConversationSeconds: 30, ExceedingSeconds: 180}}}}
	if err := ValidatePanelOptions(good, []int64{7}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePanelOptions(good, []int64{9}); ErrorCode(err) != InvalidArgument {
		t.Fatal("foreign operator accepted")
	}
	good.Operators[0].Thresholds.ConversationSeconds = 10
	if err := ValidatePanelOptions(good, []int64{7}); ErrorCode(err) != InvalidArgument {
		t.Fatal("unordered thresholds accepted")
	}
	if err := ValidatePanelOptions(PanelOptions{PipelineIDs: []int64{1, 1}}, []int64{7}); ErrorCode(err) != InvalidArgument {
		t.Fatal("duplicate pipelines accepted")
	}
}
