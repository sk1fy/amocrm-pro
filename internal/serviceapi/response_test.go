package serviceapi

import (
	"strings"
	"testing"
)

func TestResponseBudgetIsBoundedAndSuggestsSmallerPage(t *testing.T) {
	if err := ValidateResponseSize(map[string]string{"value": "small"}); err != nil {
		t.Fatal(err)
	}
	err := ValidateResponseSize(map[string]string{"value": strings.Repeat("x", MaxResponseBytes)})
	if ErrorCode(err) != ResourceExhausted || !strings.Contains(err.Error(), "smaller page") {
		t.Fatalf("large response=%v", err)
	}
}
