package amocrm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func enrichmentClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = func(s string) (*url.URL, error) { return url.Parse(s) }
	return client
}

func rejectIfCalled(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL)
	}
}

func TestEnrichmentEntityTypeAllowlist(t *testing.T) {
	client := enrichmentClient(t, rejectIfCalled(t))
	id := uuid.New()
	for _, kind := range []string{"", "task", "tasks", "catalog", "catalogs", "lead_status", "note"} {
		if _, err := client.ListNotes(context.Background(), id, kind, []int64{1}); err == nil {
			t.Fatalf("accepted notes entity %q", kind)
		} else if api := (*APIError)(nil); errors.As(err, &api) {
			t.Fatalf("allowlist used APIError: %v", err)
		}
		if _, err := client.ListCustomFields(context.Background(), id, kind); err == nil {
			t.Fatalf("accepted fields entity %q", kind)
		} else if api := (*APIError)(nil); errors.As(err, &api) {
			t.Fatalf("allowlist used APIError: %v", err)
		}
		if _, err := client.ListEntities(context.Background(), id, kind, []int64{1}); err == nil {
			t.Fatalf("accepted entities entity %q", kind)
		} else if api := (*APIError)(nil); errors.As(err, &api) {
			t.Fatalf("allowlist used APIError: %v", err)
		}
	}
}

func TestEnrichmentBatchBounds(t *testing.T) {
	client := enrichmentClient(t, rejectIfCalled(t))
	id := uuid.New()
	tooMany := make([]int64, 51)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	for _, ids := range [][]int64{nil, {}, {0}, {-1}, {1, 1}, {1, 0}, tooMany} {
		if _, err := client.ListNotes(context.Background(), id, "lead", ids); err == nil {
			t.Fatalf("accepted notes batch %#v", ids)
		} else if api := (*APIError)(nil); errors.As(err, &api) {
			t.Fatalf("batch used APIError: %v", err)
		}
		if _, err := client.ListTasks(context.Background(), id, ids); err == nil {
			t.Fatalf("accepted tasks batch %#v", ids)
		}
		if _, err := client.ListEntities(context.Background(), id, "contacts", ids); err == nil {
			t.Fatalf("accepted entities batch %#v", ids)
		}
	}
}

func TestListNotesHappyPathMapsLeadToLeads(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/leads/notes" || q.Get("limit") != "50" || q.Get("filter[id][0]") != "11" || q.Get("filter[id][1]") != "22" || q.Get("filter[id][]") != "" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"notes":[{"id":11,"entity_id":31001,"created_by":7,"updated_at":100,"note_type":"call_out","params":{"duration":37,"phone":null}}]}}`))
	})
	notes, err := client.ListNotes(context.Background(), uuid.New(), "lead", []int64{11, 22})
	if err != nil || calls != 1 || len(notes) != 1 {
		t.Fatalf("notes=%+v calls=%d err=%v", notes, calls, err)
	}
	if notes[0].ID != 11 || notes[0].EntityID != 31001 || notes[0].CreatedBy != 7 || notes[0].UpdatedAt != 100 || notes[0].NoteType != "call_out" || notes[0].EntityType != "leads" {
		t.Fatalf("mapped note=%+v", notes[0])
	}
	if !bytes.Equal(notes[0].Params, []byte(`{"duration":37,"phone":null}`)) {
		t.Fatalf("params=%s", notes[0].Params)
	}
}

func TestListNotesRejectsEmptyNextPage(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v4/leads/notes" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"notes":[]}}`))
	})
	notes, err := client.ListNotes(context.Background(), uuid.New(), "leads", []int64{11})
	if !errors.Is(err, ErrIncompleteResponse) || len(notes) != 0 || calls != 1 {
		t.Fatalf("notes=%+v calls=%d err=%v", notes, calls, err)
	}
}

func TestListNotesRejectsOversizedParams(t *testing.T) {
	exact := `"` + strings.Repeat("x", 32766) + `"`
	over := `"` + strings.Repeat("x", 32767) + `"`
	for _, tc := range []struct {
		name, params string
		accept       bool
	}{
		{"exact", exact, true},
		{"over", over, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"_embedded":{"notes":[{"id":11,"entity_id":1,"note_type":"common","params":` + tc.params + `}]}}`))
			})
			notes, err := client.ListNotes(context.Background(), uuid.New(), "lead", []int64{11})
			if tc.accept {
				if err != nil || len(notes) != 1 || len(notes[0].Params) != 32768 {
					t.Fatalf("exact params rejected: count=%d err=%v", len(notes), err)
				}
			} else if err != nil || len(notes) != 1 || !notes[0].Invalid || len(notes[0].Params) != 0 {
				t.Fatalf("oversized params accepted: count=%d err=%v", len(notes), err)
			}
		})
	}
}

func TestListNotesRejectsUnexpectedID(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_embedded":{"notes":[{"id":99,"entity_id":1,"params":{}}]}}`))
	})
	notes, err := client.ListNotes(context.Background(), uuid.New(), "lead", []int64{11})
	if !errors.Is(err, ErrIncompleteResponse) || len(notes) != 0 {
		t.Fatalf("unexpected id accepted: %+v err=%v", notes, err)
	}
}

func TestListTasksHappyPath(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/tasks" || q.Get("limit") != "50" || q.Get("filter[id][0]") != "41001" || q.Get("filter[id][1]") != "41002" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"tasks":[{"id":41001,"entity_id":31001,"entity_type":"leads","responsible_user_id":7,"text":"Call","complete_till":100,"task_type_id":2,"is_completed":true,"result":{"text":"Done"},"updated_at":200},{"id":41002,"entity_id":31002,"entity_type":"contacts","text":"Meet","result":[],"is_completed":false}]}}`))
	})
	tasks, err := client.ListTasks(context.Background(), uuid.New(), []int64{41001, 41002})
	if err != nil || calls != 1 || len(tasks) != 2 {
		t.Fatalf("tasks=%+v calls=%d err=%v", tasks, calls, err)
	}
	if tasks[0].ID != 41001 || tasks[0].EntityID != 31001 || tasks[0].EntityType != "leads" || tasks[0].ResponsibleUserID != 7 || tasks[0].Text != "Call" || tasks[0].CompleteTill != 100 || tasks[0].TaskTypeID != 2 || !tasks[0].IsCompleted || tasks[0].ResultText != "Done" || tasks[0].UpdatedAt != 200 {
		t.Fatalf("task=%+v", tasks[0])
	}
	if tasks[1].ID != 41002 || tasks[1].ResultText != "" || tasks[1].IsCompleted {
		t.Fatalf("empty result mapped=%+v", tasks[1])
	}
}

func TestListTasksRejectsEmptyNextPage(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"tasks":[]}}`))
	})
	tasks, err := client.ListTasks(context.Background(), uuid.New(), []int64{1})
	if !errors.Is(err, ErrIncompleteResponse) || len(tasks) != 0 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
}

func TestListTasksRejectsOversizedText(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_embedded":{"tasks":[{"id":1,"text":"` + strings.Repeat("x", 32769) + `"}]}}`))
	})
	tasks, err := client.ListTasks(context.Background(), uuid.New(), []int64{1})
	if err != nil || len(tasks) != 1 || !tasks[0].Invalid || tasks[0].Text != "" {
		t.Fatalf("oversized text accepted: %+v err=%v", tasks, err)
	}
}

func TestListPipelinesHappyPath(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v4/leads/pipelines" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"pipelines":[{"id":9,"name":"Sales","_embedded":{"statuses":[{"id":142,"name":"Won"},{"id":10,"name":"New"}]}}]}}`))
	})
	pipelines, err := client.ListPipelines(context.Background(), uuid.New())
	if err != nil || calls != 1 || len(pipelines) != 1 || pipelines[0].ID != 9 || pipelines[0].Name != "Sales" || len(pipelines[0].Statuses) != 2 {
		t.Fatalf("pipelines=%+v calls=%d err=%v", pipelines, calls, err)
	}
	if pipelines[0].Statuses[0] != (PipelineStatus{ID: 142, Name: "Won"}) || pipelines[0].Statuses[1] != (PipelineStatus{ID: 10, Name: "New"}) {
		t.Fatalf("statuses=%+v", pipelines[0].Statuses)
	}
}

func TestListPipelinesRejectsEmptyNextPage(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"pipelines":[]}}`))
	})
	pipelines, err := client.ListPipelines(context.Background(), uuid.New())
	if !errors.Is(err, ErrIncompleteResponse) || len(pipelines) != 0 {
		t.Fatalf("pipelines=%+v err=%v", pipelines, err)
	}
}

func TestListPipelinesRejectsBounds(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_embedded":{"pipelines":[{"id":1,"name":"` + strings.Repeat("n", 513) + `","_embedded":{"statuses":[]}}]}}`))
	})
	pipelines, err := client.ListPipelines(context.Background(), uuid.New())
	if !errors.Is(err, ErrIncompleteResponse) || len(pipelines) != 0 {
		t.Fatalf("oversized name accepted: %+v err=%v", pipelines, err)
	}
}

func TestListCustomFieldsHappyPath(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/contacts/custom_fields" || q.Get("limit") != "50" || q.Get("page") != "1" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"custom_fields":[{"id":3,"name":"Phone","type":"multitext","enums":[{"id":1,"value":"WORK"}]},{"id":4,"name":"Note","type":"text","enums":null}]}}`))
	})
	fields, err := client.ListCustomFields(context.Background(), uuid.New(), "contact")
	if err != nil || calls != 1 || len(fields) != 2 {
		t.Fatalf("fields=%+v calls=%d err=%v", fields, calls, err)
	}
	if fields[0].ID != 3 || fields[0].Name != "Phone" || fields[0].Type != "multitext" || fields[0].EntityType != "contacts" || len(fields[0].Enums) != 1 || fields[0].Enums[0] != (CustomFieldEnum{ID: 1, Value: "WORK"}) {
		t.Fatalf("field=%+v", fields[0])
	}
	if fields[1].ID != 4 || fields[1].EntityType != "contacts" || fields[1].Enums == nil || len(fields[1].Enums) != 0 {
		t.Fatalf("null enums=%+v", fields[1])
	}
}

func TestListCustomFieldsPaginatesWithoutFollowingHref(t *testing.T) {
	var pages []string
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pages = append(pages, q.Get("page"))
		if r.URL.Path != "/api/v4/leads/custom_fields" || q.Get("limit") != "50" || r.Host == "untrusted.invalid" {
			t.Errorf("unexpected request %s", r.URL)
		}
		switch q.Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/api/v4/leads/custom_fields?limit=50&page=99"}},"_embedded":{"custom_fields":[{"id":1,"name":"A","type":"text"}]}}`))
		case "2":
			_, _ = w.Write([]byte(`{"_embedded":{"custom_fields":[{"id":2,"name":"B","type":"select","enums":[{"id":8,"value":"One"}]}]}}`))
		default:
			t.Errorf("followed href page %s", r.URL)
		}
	})
	fields, err := client.ListCustomFields(context.Background(), uuid.New(), "lead")
	if err != nil || len(fields) != 2 || len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Fatalf("fields=%+v pages=%v err=%v", fields, pages, err)
	}
	if fields[0].EntityType != "leads" || fields[1].ID != 2 || fields[1].Enums[0].Value != "One" {
		t.Fatalf("mapped fields=%+v", fields)
	}
}

func TestListCustomFieldsRejectsEmptyNextPage(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v4/leads/custom_fields" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"custom_fields":[]}}`))
	})
	fields, err := client.ListCustomFields(context.Background(), uuid.New(), "leads")
	if !errors.Is(err, ErrIncompleteResponse) || len(fields) != 0 || calls != 1 {
		t.Fatalf("fields=%+v calls=%d err=%v", fields, calls, err)
	}
}

func TestListEntitiesHappyPathMapsLeadToLeads(t *testing.T) {
	var calls int
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/leads" || q.Get("limit") != "50" || q.Get("filter[id][0]") != "31001" || q.Get("filter[id][1]") != "31002" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"_embedded":{"leads":[{"id":31001,"name":"Acme"}]}}`))
	})
	entities, err := client.ListEntities(context.Background(), uuid.New(), "lead", []int64{31001, 31002})
	if err != nil || calls != 1 || len(entities) != 1 {
		t.Fatalf("entities=%+v calls=%d err=%v", entities, calls, err)
	}
	if entities[0] != (EntityName{ID: 31001, EntityType: "leads", Name: "Acme"}) {
		t.Fatalf("entity=%+v", entities[0])
	}
}

func TestListEntitiesRejectsEmptyNextPage(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/companies" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_links":{"next":{"href":"http://untrusted.invalid/secret"}},"_embedded":{"companies":[]}}`))
	})
	entities, err := client.ListEntities(context.Background(), uuid.New(), "company", []int64{1})
	if !errors.Is(err, ErrIncompleteResponse) || len(entities) != 0 {
		t.Fatalf("entities=%+v err=%v", entities, err)
	}
}

func TestListEntitiesRejectsOversizedName(t *testing.T) {
	client := enrichmentClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"_embedded":{"customers":[{"id":1,"name":"` + strings.Repeat("n", 513) + `"}]}}`))
	})
	entities, err := client.ListEntities(context.Background(), uuid.New(), "customer", []int64{1})
	if err != nil || len(entities) != 1 || !entities[0].Invalid || entities[0].Name != "" {
		t.Fatalf("oversized name accepted: %+v err=%v", entities, err)
	}
}

func TestEnrichmentIsolatesOversizedItems(t *testing.T) {
	for _, kind := range []string{"notes", "tasks", "entities"} {
		t.Run(kind, func(t *testing.T) {
			ids := make([]int64, 50)
			items := make([]map[string]any, 50)
			for i := range ids {
				ids[i] = int64(i + 1)
				items[i] = map[string]any{"id": ids[i], "entity_id": 1, "name": "valid", "text": "valid", "params": map[string]string{"text": "valid"}}
			}
			switch kind {
			case "notes":
				items[49]["params"] = map[string]string{"text": strings.Repeat("x", 32769)}
			case "tasks":
				items[49]["text"] = strings.Repeat("x", 32769)
			case "entities":
				items[49]["name"] = strings.Repeat("x", 513)
			}
			key := kind
			if kind == "entities" {
				key = "leads"
			}
			client := enrichmentClient(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"_embedded": map[string]any{key: items}})
			})
			valid, invalid := 0, 0
			switch kind {
			case "notes":
				got, err := client.ListNotes(context.Background(), uuid.New(), "leads", ids)
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range got {
					if o.Invalid {
						invalid++
						if o.ID != 50 || len(o.Params) != 0 {
							t.Fatal("invalid note leaks content")
						}
					} else {
						valid++
					}
				}
			case "tasks":
				got, err := client.ListTasks(context.Background(), uuid.New(), ids)
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range got {
					if o.Invalid {
						invalid++
						if o.ID != 50 || o.Text != "" {
							t.Fatal("invalid task leaks content")
						}
					} else {
						valid++
					}
				}
			case "entities":
				got, err := client.ListEntities(context.Background(), uuid.New(), "leads", ids)
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range got {
					if o.Invalid {
						invalid++
						if o.ID != 50 || o.Name != "" {
							t.Fatal("invalid entity leaks content")
						}
					} else {
						valid++
					}
				}
			}
			if valid != 49 || invalid != 1 {
				t.Fatalf("valid=%d invalid=%d", valid, invalid)
			}
		})
	}
}
