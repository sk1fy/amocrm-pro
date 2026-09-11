package serviceapi

import "testing"

func TestValidateDisplayWindowAndEmployees(t *testing.T) {
	if err := ValidateDisplayWindow(DisplayWindow{From: "09:00", To: "18:00"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDisplayWindow(DisplayWindow{From: "09:00", To: "09:00"}); err == nil {
		t.Fatal("expected identical bounds to fail")
	}
	if err := ValidateDisplayWindow(DisplayWindow{From: "9:00", To: "18:00"}); err == nil {
		t.Fatal("expected short hour to fail")
	}
	if err := ValidateEmployeeIDs([]int64{7, 9}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEmployeeIDs([]int64{7, 7}); err == nil {
		t.Fatal("expected duplicate ids to fail")
	}
	if err := ValidatePanelName("Смена А"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePanelName(" Смена"); err == nil {
		t.Fatal("expected surrounding whitespace to fail")
	}
}
