package serviceapi

import (
	"encoding/json"
	"os"
	"testing"
)

func TestEventCategoryMatchesFixtureFamilies(t *testing.T) {
	raw, err := os.ReadFile("../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Category string `json:"category"`
			Input    struct {
				Type string `json:"type"`
			} `json:"input"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 29 {
		t.Fatalf("fixture coverage changed: %d", len(fixture.Cases))
	}
	for _, c := range fixture.Cases {
		if got := EventCategory(c.Input.Type); got != c.Category {
			t.Fatalf("%s type %s: category %s want %s", c.Input.Type, c.Input.Type, got, c.Category)
		}
		if title := EventTitle(c.Input.Type); title == "" || title != EventTitle(c.Input.Type) {
			t.Fatalf("unstable title for %s", c.Input.Type)
		}
	}
}

func TestUnknownTypeStaysNeutralAndOriginal(t *testing.T) {
	if EventCategory("synthetic_future_event_v9") != CategoryOther {
		t.Fatal(EventCategory("synthetic_future_event_v9"))
	}
	if EventTitle("synthetic_future_event_v9") != "Событие synthetic_future_event_v9" {
		t.Fatal(EventTitle("synthetic_future_event_v9"))
	}
	if AuthorLabel(0, nil) != "Автор не указан" || AuthorLabel(71999, nil) != "Пользователь #71999" {
		t.Fatal(AuthorLabel(0, nil), AuthorLabel(71999, nil))
	}
}

func TestCustomFieldAndRelationPatterns(t *testing.T) {
	if EventCategory("custom_field_91001_value_changed") != CategoryCustomFields {
		t.Fatal("dynamic custom field")
	}
	if EventCategory("contact_linked") != CategoryRelations || EventCategory("company_unlinked") != CategoryRelations {
		t.Fatal("relations")
	}
	if EventTitle("custom_field_91001_value_changed") != "Изменено поле #91001" {
		t.Fatal(EventTitle("custom_field_91001_value_changed"))
	}
}

func TestRequirePresentationVersion(t *testing.T) {
	q := Query{From: 1, To: 2, Categories: []string{CategoryTasks}}
	if err := RequireQueryVersion(q, QueryResult{ReadVersion: EventReadVersion}); ErrorCode(err) != Unavailable {
		t.Fatal(err)
	}
	if err := RequireQueryVersion(q, QueryResult{ReadVersion: PresentationReadVersion}); err != nil {
		t.Fatal(err)
	}
	legacy := Query{From: 1, To: 2}
	if err := RequireQueryVersion(legacy, QueryResult{}); err != nil {
		t.Fatal(err)
	}
	if err := RequireQueryVersion(Query{From: 1, To: 2, GroupID: 9}, QueryResult{ReadVersion: EventReadVersion}); ErrorCode(err) != Unavailable {
		t.Fatal(err)
	}
}
