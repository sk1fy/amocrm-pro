package gateway

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"strings"
	"testing"
	"time"
)

type countingPolicy struct {
	serviceapi.Policy
	scope serviceapi.Scope
	err   error
	calls int
}

func (p *countingPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	p.calls++
	if p.err != nil {
		return serviceapi.Principal{}, p.err
	}
	return serviceapi.Principal{Scope: p.scope}, nil
}

type enrichmentAPI struct {
	API
	err                                       error
	notes, tasks, pipelines, fields, entities int
	noteType, fieldType, entityType           string
	ids                                       []int64
	notesOut                                  []amocrm.Note
	tasksOut                                  []amocrm.Task
	pipelinesOut                              []amocrm.Pipeline
	fieldsOut                                 []amocrm.CustomField
	entitiesOut                               []amocrm.EntityName
}

func (a *enrichmentAPI) ListNotes(_ context.Context, _ uuid.UUID, entityType string, ids []int64) ([]amocrm.Note, error) {
	a.notes++
	a.noteType = entityType
	a.ids = append([]int64(nil), ids...)
	if a.err != nil {
		return nil, a.err
	}
	return a.notesOut, nil
}
func (a *enrichmentAPI) ListTasks(_ context.Context, _ uuid.UUID, ids []int64) ([]amocrm.Task, error) {
	a.tasks++
	a.ids = append([]int64(nil), ids...)
	if a.err != nil {
		return nil, a.err
	}
	return a.tasksOut, nil
}
func (a *enrichmentAPI) ListPipelines(context.Context, uuid.UUID) ([]amocrm.Pipeline, error) {
	a.pipelines++
	if a.err != nil {
		return nil, a.err
	}
	return a.pipelinesOut, nil
}
func (a *enrichmentAPI) ListCustomFields(_ context.Context, _ uuid.UUID, entityType string) ([]amocrm.CustomField, error) {
	a.fields++
	a.fieldType = entityType
	if a.err != nil {
		return nil, a.err
	}
	return a.fieldsOut, nil
}
func (a *enrichmentAPI) ListEntities(_ context.Context, _ uuid.UUID, entityType string, ids []int64) ([]amocrm.EntityName, error) {
	a.entities++
	a.entityType = entityType
	a.ids = append([]int64(nil), ids...)
	if a.err != nil {
		return nil, a.err
	}
	return a.entitiesOut, nil
}

func enrichmentService(api API, deny bool) (*Service, *countingPolicy, *enrichmentAPI) {
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	policy := &countingPolicy{scope: scope}
	if deny {
		policy.err = serviceapi.Fail(serviceapi.PermissionDenied, "denied")
	}
	fake, _ := api.(*enrichmentAPI)
	if fake == nil {
		fake = &enrichmentAPI{}
		api = fake
	}
	return New(api, policy), policy, fake
}

func TestEnrichmentBoundsRejectInvalidIDsAndEntityTypes(t *testing.T) {
	api := &enrichmentAPI{}
	service, policy, _ := enrichmentService(api, false)
	tooMany := make([]int64, serviceapi.EnrichmentBatchLimit+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	cases := []struct {
		name string
		call func() error
	}{
		{"notes empty", func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads"})
			return err
		}},
		{"notes 51", func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads", IDs: tooMany})
			return err
		}},
		{"notes duplicate", func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads", IDs: []int64{1, 1}})
			return err
		}},
		{"notes zero", func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads", IDs: []int64{0}})
			return err
		}},
		{"notes unknown", func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "task", IDs: []int64{1}})
			return err
		}},
		{"tasks empty", func() error { _, err := service.Tasks(context.Background(), serviceapi.TasksRequest{}); return err }},
		{"tasks 51", func() error {
			_, err := service.Tasks(context.Background(), serviceapi.TasksRequest{IDs: tooMany})
			return err
		}},
		{"tasks duplicate", func() error {
			_, err := service.Tasks(context.Background(), serviceapi.TasksRequest{IDs: []int64{1, 1}})
			return err
		}},
		{"tasks zero", func() error {
			_, err := service.Tasks(context.Background(), serviceapi.TasksRequest{IDs: []int64{0}})
			return err
		}},
		{"fields unknown", func() error {
			_, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "task"})
			return err
		}},
		{"entities empty", func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "leads"})
			return err
		}},
		{"entities 51", func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "leads", IDs: tooMany})
			return err
		}},
		{"entities duplicate", func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "leads", IDs: []int64{1, 1}})
			return err
		}},
		{"entities zero", func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "leads", IDs: []int64{0}})
			return err
		}},
		{"entities unknown", func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "task", IDs: []int64{1}})
			return err
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if serviceapi.ErrorCode(tt.call()) != serviceapi.InvalidArgument {
				t.Fatal("expected invalid_argument")
			}
		})
	}
	if api.notes != 0 || api.tasks != 0 || api.pipelines != 0 || api.fields != 0 || api.entities != 0 || policy.calls != 0 {
		t.Fatalf("invalid args reached policy or API notes=%d tasks=%d pipelines=%d fields=%d entities=%d policy=%d", api.notes, api.tasks, api.pipelines, api.fields, api.entities, policy.calls)
	}
}

func TestEnrichmentNotesNormalizeEntityType(t *testing.T) {
	api := &enrichmentAPI{notesOut: []amocrm.Note{{ID: 9, EntityID: 42, EntityType: "lead", NoteType: "common", CreatedBy: 7, UpdatedAt: 100, Params: json.RawMessage(`{"text":"hi"}`)}}}
	service, _, _ := enrichmentService(api, false)
	page, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "lead", IDs: []int64{9}})
	if err != nil || api.noteType != "leads" || len(page.Notes) != 1 || page.Notes[0].EntityType != "leads" || page.Notes[0].ID != 9 {
		t.Fatalf("notes=%+v apiType=%s err=%v", page, api.noteType, err)
	}
}

func TestEnrichmentPolicyDenyDoesNotCallAPI(t *testing.T) {
	api := &enrichmentAPI{
		notesOut:     []amocrm.Note{{ID: 1}},
		tasksOut:     []amocrm.Task{{ID: 1}},
		pipelinesOut: []amocrm.Pipeline{{ID: 1, Name: "Sales"}},
		fieldsOut:    []amocrm.CustomField{{ID: 1, Name: "City"}},
		entitiesOut:  []amocrm.EntityName{{ID: 1, Name: "Lead"}},
	}
	service, policy, _ := enrichmentService(api, true)
	calls := []func() error{
		func() error {
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads", IDs: []int64{1}})
			return err
		},
		func() error {
			_, err := service.Tasks(context.Background(), serviceapi.TasksRequest{IDs: []int64{1}})
			return err
		},
		func() error {
			_, err := service.Pipelines(context.Background(), serviceapi.CatalogRequest{})
			return err
		},
		func() error {
			_, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "leads"})
			return err
		},
		func() error {
			_, err := service.Entities(context.Background(), serviceapi.EntitiesRequest{EntityType: "leads", IDs: []int64{1}})
			return err
		},
	}
	for _, call := range calls {
		if serviceapi.ErrorCode(call()) != serviceapi.PermissionDenied {
			t.Fatal("expected permission_denied")
		}
	}
	if policy.calls != len(calls) || api.notes != 0 || api.tasks != 0 || api.pipelines != 0 || api.fields != 0 || api.entities != 0 {
		t.Fatalf("deny leaked to API notes=%d tasks=%d pipelines=%d fields=%d entities=%d policy=%d", api.notes, api.tasks, api.pipelines, api.fields, api.entities, policy.calls)
	}
}

func TestEnrichmentMapsUpstreamErrors(t *testing.T) {
	cases := []struct {
		kind amocrm.ErrorKind
		want serviceapi.Code
	}{{amocrm.ErrorNotFound, serviceapi.NotFound}, {amocrm.ErrorForbidden, serviceapi.PermissionDenied}, {amocrm.ErrorRateLimited, serviceapi.ResourceExhausted}}
	for _, tt := range cases {
		t.Run(string(tt.kind), func(t *testing.T) {
			api := &enrichmentAPI{err: &amocrm.APIError{Kind: tt.kind, RetryAfter: 13 * time.Second}}
			service, _, _ := enrichmentService(api, false)
			_, err := service.Notes(context.Background(), serviceapi.NotesRequest{EntityType: "leads", IDs: []int64{1}})
			if serviceapi.ErrorCode(err) != tt.want {
				t.Fatalf("got %v want %s", err, tt.want)
			}
			if tt.kind == amocrm.ErrorRateLimited {
				domain, _ := err.(*serviceapi.Error)
				if domain == nil || domain.RetryAfter != 13*time.Second {
					t.Fatalf("retry-after=%v", err)
				}
			}
		})
	}
}

func TestEnrichmentPipelinesCacheHitsWithinTTLStillValidate(t *testing.T) {
	api := &enrichmentAPI{pipelinesOut: []amocrm.Pipeline{{ID: 3, Name: "Sales", Statuses: []amocrm.PipelineStatus{{ID: 7, Name: "New"}}}}, fieldsOut: []amocrm.CustomField{{ID: 8, Name: "City", Type: "text", Enums: []amocrm.CustomFieldEnum{{ID: 1, Value: "A"}}}}}
	service, policy, _ := enrichmentService(api, false)
	first, err := service.Pipelines(context.Background(), serviceapi.CatalogRequest{})
	if err != nil || first.FetchedAt == 0 || len(first.Pipelines) != 1 || first.Pipelines[0].Statuses[0].Name != "New" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.Pipelines(context.Background(), serviceapi.CatalogRequest{})
	if err != nil || api.pipelines != 1 || policy.calls != 2 || second.FetchedAt != first.FetchedAt {
		t.Fatalf("cache pipelines=%d policy=%d second=%+v err=%v", api.pipelines, policy.calls, second, err)
	}
	fields, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "lead"})
	if err != nil || api.fieldType != "leads" || len(fields.Fields) != 1 || fields.Fields[0].EntityType != "leads" || fields.FetchedAt == 0 {
		t.Fatalf("fields=%+v type=%s err=%v", fields, api.fieldType, err)
	}
	if _, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "leads"}); err != nil || api.fields != 1 || policy.calls != 4 {
		t.Fatalf("fields cache hits=%d policy=%d err=%v", api.fields, policy.calls, err)
	}
	if _, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "contacts"}); err != nil || api.fields != 2 {
		t.Fatalf("fields keyed by entity type hits=%d err=%v", api.fields, err)
	}
}

func TestOversizedCatalogIsRejectedBeforeCaching(t *testing.T) {
	large := make([]amocrm.Pipeline, 0, 200)
	for i := 0; i < 200; i++ {
		large = append(large, amocrm.Pipeline{ID: int64(i + 1), Name: strings.Repeat("n", 3000)})
	}
	api := &enrichmentAPI{pipelinesOut: large}
	service, _, _ := enrichmentService(api, false)
	if _, err := service.Pipelines(context.Background(), serviceapi.CatalogRequest{}); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
		t.Fatalf("oversized catalog=%v", err)
	}
	if len(service.pipelines) != 0 {
		t.Fatal("oversized catalog entered cache")
	}
	api.pipelinesOut = []amocrm.Pipeline{{ID: 1, Name: "Sales"}}
	result, err := service.Pipelines(context.Background(), serviceapi.CatalogRequest{})
	if err != nil || len(result.Pipelines) != 1 || api.pipelines != 2 || len(service.pipelines) != 1 {
		t.Fatalf("recovery=%+v calls=%d cache=%d err=%v", result, api.pipelines, len(service.pipelines), err)
	}
	largeFields := make([]amocrm.CustomField, 0, 200)
	for i := 0; i < 200; i++ {
		largeFields = append(largeFields, amocrm.CustomField{ID: int64(i + 1), Name: strings.Repeat("n", 3000)})
	}
	api.fieldsOut = largeFields
	if _, err := service.CustomFields(context.Background(), serviceapi.CustomFieldsRequest{EntityType: "leads"}); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
		t.Fatalf("oversized fields=%v", err)
	}
	if len(service.fields) != 0 {
		t.Fatal("oversized fields entered cache")
	}
}

func TestEnrichmentReturnsInvalidIDsSeparately(t *testing.T) {
	api := &enrichmentAPI{notesOut: []amocrm.Note{{ID: 1}, {ID: 2, Invalid: true}}, tasksOut: []amocrm.Task{{ID: 1}, {ID: 2, Invalid: true}}, entitiesOut: []amocrm.EntityName{{ID: 1}, {ID: 2, Invalid: true}}}
	s, _, _ := enrichmentService(api, false)
	ctx := context.Background()
	notes, err := s.Notes(ctx, serviceapi.NotesRequest{EntityType: "leads", IDs: []int64{1, 2}})
	if err != nil || len(notes.Notes) != 1 || len(notes.InvalidIDs) != 1 || notes.InvalidIDs[0] != 2 {
		t.Fatalf("notes %+v %v", notes, err)
	}
	tasks, err := s.Tasks(ctx, serviceapi.TasksRequest{IDs: []int64{1, 2}})
	if err != nil || len(tasks.Tasks) != 1 || len(tasks.InvalidIDs) != 1 || tasks.InvalidIDs[0] != 2 {
		t.Fatalf("tasks %+v %v", tasks, err)
	}
	entities, err := s.Entities(ctx, serviceapi.EntitiesRequest{EntityType: "leads", IDs: []int64{1, 2}})
	if err != nil || len(entities.Entities) != 1 || len(entities.InvalidIDs) != 1 || entities.InvalidIDs[0] != 2 {
		t.Fatalf("entities %+v %v", entities, err)
	}
}
