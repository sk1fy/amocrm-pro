package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestListOpenTasksPageFiltersByActorAndRejectsForeignRows(t *testing.T) {
	var query string
	body := `{"_embedded":{"tasks":[{"id":5,"entity_id":9,"entity_type":"leads","responsible_user_id":7,"complete_till":100,"task_type_id":1,"text":"Call","is_completed":false}]},"_links":{"next":{"href":"next"}}}`
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	page, err := client.ListOpenTasksPage(context.Background(), uuid.New(), 7, 2)
	if err != nil || len(page.Tasks) != 1 || !page.HasNext || page.Tasks[0].Text != "Call" || page.Tasks[0].EntityID != 9 {
		t.Fatalf("page %+v %v", page, err)
	}
	for _, want := range []string{"filter%5Bresponsible_user_id%5D=7", "filter%5Bis_completed%5D=0", "order%5Bcomplete_till%5D=asc", "limit=250", "page=2"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q lacks %s", query, want)
		}
	}
	// A foreign or completed row means amoCRM ignored the filter: fail closed.
	for _, row := range []string{`"responsible_user_id":8,"is_completed":false`, `"responsible_user_id":7,"is_completed":true`} {
		body = `{"_embedded":{"tasks":[{"id":5,` + row + `}]}}`
		if _, err := client.ListOpenTasksPage(context.Background(), uuid.New(), 7, 1); !errors.Is(err, ErrIncompleteResponse) {
			t.Fatalf("accepted %s: %v", row, err)
		}
	}
	if _, err := client.ListOpenTasksPage(context.Background(), uuid.New(), 0, 1); err == nil {
		t.Fatal("accepted missing actor")
	}
}

func TestListOpenTasksPageNoContentIsEmptyQueue(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	page, err := client.ListOpenTasksPage(context.Background(), uuid.New(), 7, 1)
	if err != nil || page.HasNext || page.Tasks == nil || len(page.Tasks) != 0 {
		t.Fatalf("empty page %+v %v", page, err)
	}
}

func TestHasOpenTaskFiltersByActorAndCardAndFailsClosed(t *testing.T) {
	var query string
	status, body := http.StatusOK, `{"_embedded":{"tasks":[{"id":5,"entity_id":9,"entity_type":"leads","responsible_user_id":7,"is_completed":false}]}}`
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	ok, err := client.HasOpenTask(context.Background(), uuid.New(), 7, "lead", 9)
	if err != nil || !ok {
		t.Fatalf("open task %v %v", ok, err)
	}
	for _, want := range []string{"filter%5Bresponsible_user_id%5D=7", "filter%5Bis_completed%5D=0", "filter%5Bentity_type%5D=leads", "filter%5Bentity_id%5D%5B%5D=9", "limit=1"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q lacks %s", query, want)
		}
	}
	// Any row outside the filter means amoCRM ignored it: never visible.
	for _, row := range []string{
		`"entity_id":9,"entity_type":"leads","responsible_user_id":8,"is_completed":false`,
		`"entity_id":9,"entity_type":"leads","responsible_user_id":7,"is_completed":true`,
		`"entity_id":10,"entity_type":"leads","responsible_user_id":7,"is_completed":false`,
		`"entity_id":9,"entity_type":"contacts","responsible_user_id":7,"is_completed":false`,
	} {
		body = `{"_embedded":{"tasks":[{"id":5,` + row + `}]}}`
		if ok, err := client.HasOpenTask(context.Background(), uuid.New(), 7, "leads", 9); ok || !errors.Is(err, ErrIncompleteResponse) {
			t.Fatalf("accepted %s: %v %v", row, ok, err)
		}
	}
	status = http.StatusNoContent
	if ok, err := client.HasOpenTask(context.Background(), uuid.New(), 7, "leads", 9); ok || err != nil {
		t.Fatalf("204 %v %v", ok, err)
	}
	if _, err := client.HasOpenTask(context.Background(), uuid.New(), 0, "leads", 9); err == nil {
		t.Fatal("accepted missing actor")
	}
	if _, err := client.HasOpenTask(context.Background(), uuid.New(), 7, "catalogs", 9); err == nil {
		t.Fatal("accepted unsupported entity type")
	}
}

func TestCompleteTaskSendsCompletionAndResult(t *testing.T) {
	var method, path string
	var body map[string]any
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"id":42,"updated_at":1}`)
	})
	if err := client.CompleteTask(context.Background(), uuid.New(), 42, "Done"); err != nil {
		t.Fatal(err)
	}
	result, _ := body["result"].(map[string]any)
	if method != http.MethodPatch || path != "/api/v4/tasks/42" || body["is_completed"] != true || result["text"] != "Done" {
		t.Fatalf("PATCH %s %s %+v", method, path, body)
	}
}

func TestCompleteTaskSurfacesRateLimitWithoutRetry(t *testing.T) {
	calls := 0
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	err := client.CompleteTask(context.Background(), uuid.New(), 42, "Done")
	var api *APIError
	if !errors.As(err, &api) || api.Kind != ErrorRateLimited || api.RetryAfter.Seconds() != 7 || calls != 1 {
		t.Fatalf("429 %v calls=%d", err, calls)
	}
}

func TestEntityContextNoContentIsNotFound(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	_, err := client.GetEntityContext(context.Background(), uuid.New(), "leads", 3)
	var api *APIError
	if !errors.As(err, &api) || api.Kind != ErrorNotFound {
		t.Fatalf("deleted card: %v", err)
	}
}

func TestEntityContextReadsLeadPipelineOnly(t *testing.T) {
	var path string
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = io.WriteString(w, `{"id":3,"name":"Deal","responsible_user_id":7,"pipeline_id":11,"status_id":12}`)
	})
	lead, err := client.GetEntityContext(context.Background(), uuid.New(), "lead", 3)
	if err != nil || path != "/api/v4/leads/3" || lead.Name != "Deal" || lead.PipelineID != 11 || lead.StatusID != 12 || lead.ResponsibleUserID != 7 {
		t.Fatalf("lead %+v %v", lead, err)
	}
	contact, err := client.GetEntityContext(context.Background(), uuid.New(), "contacts", 3)
	if err != nil || contact.PipelineID != 0 || contact.StatusID != 0 {
		t.Fatalf("contact carries lead pipeline: %+v %v", contact, err)
	}
	if _, err := client.GetEntityContext(context.Background(), uuid.New(), "catalogs", 3); err == nil {
		t.Fatal("accepted unsupported entity type")
	}
}

func TestLeadParticipantsAndParticipantLeads(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/leads/3":
			if r.URL.Query().Get("with") != "contacts" {
				t.Errorf("lead without contacts: %s", r.URL)
			}
			_, _ = io.WriteString(w, `{"id":3,"_embedded":{"contacts":[{"id":4},{"id":0}],"companies":[{"id":5}]}}`)
		case "/api/v4/contacts/4":
			_, _ = io.WriteString(w, `{"id":4,"name":"Anna","_embedded":{"leads":[{"id":3},{"id":6}]}}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	contacts, companies, err := client.LeadParticipants(context.Background(), uuid.New(), 3)
	if err != nil || len(contacts) != 1 || contacts[0] != 4 || len(companies) != 1 || companies[0] != 5 {
		t.Fatalf("participants %v %v %v", contacts, companies, err)
	}
	name, leads, err := client.ParticipantLeads(context.Background(), uuid.New(), "contacts", 4)
	if err != nil || name != "Anna" || len(leads) != 2 {
		t.Fatalf("participant %q %v %v", name, leads, err)
	}
	if _, _, err := client.ParticipantLeads(context.Background(), uuid.New(), "leads", 4); err == nil {
		t.Fatal("lead accepted as participant")
	}
}

func TestListTaskTypesAndPipelineStatusColor(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/account" {
			_, _ = io.WriteString(w, `{"_embedded":{"task_types":[{"id":1,"name":"Звонок"},{"id":2,"name":"Встреча"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"_embedded":{"pipelines":[{"id":11,"name":"Sales","_embedded":{"statuses":[{"id":12,"name":"New","color":"#99ccff"}]}}]}}`)
	})
	types, err := client.ListTaskTypes(context.Background(), uuid.New())
	if err != nil || len(types) != 2 || types[0].Name != "Звонок" {
		t.Fatalf("types %+v %v", types, err)
	}
	pipelines, err := client.ListPipelines(context.Background(), uuid.New())
	if err != nil || pipelines[0].Statuses[0].Color != "#99ccff" {
		t.Fatalf("pipelines %+v %v", pipelines, err)
	}
}
