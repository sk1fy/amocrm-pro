package serviceapi

import "encoding/json"

const (
	ActionNotes        = "notes"
	ActionTasks        = "tasks"
	ActionPipelines    = "pipelines"
	ActionCustomFields = "custom_fields"
	ActionEntities     = "entities"
)

const (
	EnrichmentPending     = "pending"
	EnrichmentReady       = "ready"
	EnrichmentUnavailable = "unavailable"
	EnrichmentRetry       = "retry"
	EnrichmentError       = "error"
)

const (
	ReasonNotLoaded        = "not_loaded"
	ReasonMissingInSource  = "missing_in_source"
	ReasonNotFound         = "not_found"
	ReasonPermissionDenied = "permission_denied"
	ReasonDeleted          = "deleted"
	ReasonTemporary        = "temporary"
	ReasonUnsupported      = "unsupported"
	ReasonInvalid          = "invalid"
)

const (
	ObjectNote        = "note"
	ObjectTask        = "task"
	ObjectEntity      = "entity"
	ObjectPipeline    = "pipeline"
	ObjectCustomField = "custom_field"
)

const (
	SourceNotesAPI        = "notes_api"
	SourceTasksAPI        = "tasks_api"
	SourcePipelinesAPI    = "pipelines_api"
	SourceCustomFieldsAPI = "custom_fields_api"
	SourceEntitiesAPI     = "entities_api"
	SourceEventPayload    = "event_payload"
	SourceDirectory       = "directory"
)

const EnrichmentBatchLimit = 50
const EnrichmentPayloadLimit = 32768

type NotesRequest struct {
	Auth       Auth    `json:"auth"`
	EntityType string  `json:"entity_type"`
	IDs        []int64 `json:"ids"`
}
type Note struct {
	ID         int64           `json:"id"`
	EntityID   int64           `json:"entity_id"`
	EntityType string          `json:"entity_type"`
	NoteType   string          `json:"note_type"`
	CreatedBy  int64           `json:"created_by"`
	UpdatedAt  int64           `json:"updated_at"`
	Params     json.RawMessage `json:"params"`
}
type NotePage struct {
	InvalidIDs []int64 `json:"invalid_ids,omitempty"`
	Notes      []Note  `json:"notes"`
}

type TasksRequest struct {
	Auth Auth    `json:"auth"`
	IDs  []int64 `json:"ids"`
}
type Task struct {
	ID                int64  `json:"id"`
	EntityID          int64  `json:"entity_id"`
	EntityType        string `json:"entity_type"`
	ResponsibleUserID int64  `json:"responsible_user_id"`
	Text              string `json:"text"`
	CompleteTill      int64  `json:"complete_till"`
	TaskTypeID        int64  `json:"task_type_id"`
	IsCompleted       bool   `json:"is_completed"`
	ResultText        string `json:"result_text"`
	UpdatedAt         int64  `json:"updated_at"`
}
type TaskPage struct {
	InvalidIDs []int64 `json:"invalid_ids,omitempty"`
	Tasks      []Task  `json:"tasks"`
}

type CatalogRequest struct {
	Auth Auth `json:"auth"`
}
type PipelineStatus struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}
type Pipeline struct {
	ID       int64            `json:"id"`
	Name     string           `json:"name"`
	Statuses []PipelineStatus `json:"statuses"`
}
type PipelineCatalog struct {
	FetchedAt int64      `json:"fetched_at"`
	Pipelines []Pipeline `json:"pipelines"`
}

type CustomFieldsRequest struct {
	Auth       Auth   `json:"auth"`
	EntityType string `json:"entity_type"`
}
type CustomFieldEnum struct {
	ID    int64  `json:"id"`
	Value string `json:"value"`
}
type CustomField struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	EntityType string            `json:"entity_type"`
	Enums      []CustomFieldEnum `json:"enums"`
}
type CustomFieldCatalog struct {
	FetchedAt int64         `json:"fetched_at"`
	Fields    []CustomField `json:"fields"`
}

type EntitiesRequest struct {
	Auth       Auth    `json:"auth"`
	EntityType string  `json:"entity_type"`
	IDs        []int64 `json:"ids"`
}
type EntityName struct {
	ID         int64  `json:"id"`
	EntityType string `json:"entity_type"`
	Name       string `json:"name"`
}
type EntityCatalog struct {
	InvalidIDs []int64      `json:"invalid_ids,omitempty"`
	Entities   []EntityName `json:"entities"`
}

type EnrichmentObject struct {
	Current    bool            `json:"current"`
	ObjectKind string          `json:"object_kind"`
	ObjectKey  string          `json:"object_key"`
	State      string          `json:"state"`
	ReasonCode string          `json:"reason_code,omitempty"`
	Source     string          `json:"source"`
	FetchedAt  int64           `json:"fetched_at,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}
type CatalogName struct {
	Current    bool   `json:"current"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	State      string `json:"state"`
	EntityType string `json:"entity_type,omitempty"`
	ID         int64  `json:"id"`
}

func CatalogEntityType(kind string) (string, bool) {
	switch kind {
	case "lead", "leads":
		return "leads", true
	case "contact", "contacts":
		return "contacts", true
	case "company", "companies":
		return "companies", true
	case "customer", "customers":
		return "customers", true
	default:
		return "", false
	}
}

func ValidateIDBatch(ids []int64, limit int) error {
	if len(ids) == 0 || len(ids) > limit {
		return Fail(InvalidArgument, "id batch is empty or exceeds the enrichment limit")
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return Fail(InvalidArgument, "ids must be distinct and positive")
		}
		seen[id] = true
	}
	return nil
}

func HistoricalEvent(e Event) Event {
	e.Enrichment = nil
	e.Names = nil
	e.View = nil
	return e
}
