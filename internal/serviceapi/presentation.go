package serviceapi

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// PresentationReadVersion is required when category filters, unknown-author
// inclusion or time buckets are requested. Version 2 readers ignore them.
const PresentationReadVersion = 3

// InterpretationVersion identifies Activity classification and labels.
// Aggregates are computed live, so a new version does not rewrite stored history.
const InterpretationVersion = 1

const (
	CategoryTasks        = "tasks"
	CategoryCalls        = "calls"
	CategoryNotes        = "notes"
	CategoryMessages     = "messages"
	CategoryStatus       = "status"
	CategoryBudget       = "budget"
	CategoryResponsible  = "responsible"
	CategoryCustomFields = "custom_fields"
	CategoryRelations    = "relations"
	CategoryAttachments  = "attachments"
	CategoryOther        = "other"
)

const (
	DetailOmitted     = "omitted"
	DetailAvailable   = "available"
	DetailPending     = "pending"
	DetailUnavailable = "unavailable"
	DetailMixed       = "mixed"
)

const (
	CoverageUnknown  = "unknown"
	CoveragePartial  = "partial"
	CoverageVerified = "verified"
	CoverageStale    = "stale"
)

const (
	FreshnessCurrent        = "current"
	FreshnessLagging        = "lagging"
	FreshnessError          = "error"
	FreshnessReauthRequired = "reauth_required"
	EmptyReasonNoEvents     = "no_events"
	EmptyReasonUnverified   = "unverified_empty"
	BucketHour              = "hour"
	BucketDay               = "day"
	BucketAuto              = "auto"
	BucketNone              = "none"
)

const MaxHourBuckets = 48
const MaxDayBuckets = 31

type EventDetail struct {
	Key     string          `json:"key,omitempty"`
	Label   string          `json:"label,omitempty"`
	Before  json.RawMessage `json:"before,omitempty"`
	After   json.RawMessage `json:"after,omitempty"`
	Text    string          `json:"text,omitempty"`
	Source  string          `json:"source,omitempty"`
	Current bool            `json:"current,omitempty"`
}

type EventView struct {
	Category        string        `json:"category,omitempty"`
	Title           string        `json:"title,omitempty"`
	Summary         string        `json:"summary,omitempty"`
	DetailState     string        `json:"detail_state,omitempty"`
	EnrichmentState string        `json:"enrichment_state,omitempty"`
	AuthorLabel     string        `json:"author_label,omitempty"`
	EntityLabel     string        `json:"entity_label,omitempty"`
	Details         []EventDetail `json:"details,omitempty"`
}

type CategoryCount struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

type TimeBucket struct {
	StartAt  int64  `json:"start_at"`
	EndAt    int64  `json:"end_at"`
	Count    int64  `json:"count"`
	Coverage string `json:"coverage"`
}

type QueryTotals struct {
	UniqueEvents         int64           `json:"unique_events"`
	EntityCount          int64           `json:"entity_count"`
	TaskCompletedEvents  int64           `json:"task_completed_events"`
	UniqueCompletedTasks int64           `json:"unique_completed_tasks"`
	FirstEventAt         int64           `json:"first_event_at,omitempty"`
	LastEventAt          int64           `json:"last_event_at,omitempty"`
	CategoryCounts       []CategoryCount `json:"category_counts,omitempty"`
}

// KnownCategories is the closed product set from BASE-03. ACT-02 families are
// these categories, not a second independent taxonomy.
func KnownCategories() []string {
	return []string{CategoryTasks, CategoryCalls, CategoryNotes, CategoryMessages, CategoryStatus, CategoryBudget, CategoryResponsible, CategoryCustomFields, CategoryRelations, CategoryAttachments, CategoryOther}
}

func ValidCategory(s string) bool {
	for _, cat := range KnownCategories() {
		if s == cat {
			return true
		}
	}
	return false
}

// ExactCategoryTypes lists closed event types per category. Dynamic custom
// fields and link/unlink suffixes are matched separately.
func ExactCategoryTypes() map[string][]string {
	return map[string][]string{
		CategoryTasks:        {"task_added", "task_completed", "task_deleted", "task_text_changed", "task_deadline_changed", "task_type_changed", "task_result_added"},
		CategoryCalls:        {"incoming_call", "outgoing_call", "call_in", "call_out"},
		CategoryNotes:        {"common_note_added", "common_note_deleted", "service_note_added"},
		CategoryMessages:     {"incoming_chat_message", "outgoing_chat_message", "entity_direct_message", "outgoing_sms", "incoming_sms"},
		CategoryStatus:       {"lead_status_changed"},
		CategoryBudget:       {"sale_field_changed"},
		CategoryResponsible:  {"entity_responsible_changed"},
		CategoryCustomFields: {"custom_field_value_changed"},
		CategoryAttachments:  {"attachment_note_added"},
	}
}

func IsCustomFieldType(eventType string) bool {
	if eventType == "custom_field_value_changed" {
		return true
	}
	return strings.HasPrefix(eventType, "custom_field_") && strings.HasSuffix(eventType, "_value_changed")
}

func IsRelationType(eventType string) bool {
	return strings.HasSuffix(eventType, "_linked") || strings.HasSuffix(eventType, "_unlinked")
}

func EventCategory(eventType string) string {
	for category, types := range ExactCategoryTypes() {
		for _, kind := range types {
			if eventType == kind {
				return category
			}
		}
	}
	if IsCustomFieldType(eventType) {
		return CategoryCustomFields
	}
	if IsRelationType(eventType) {
		return CategoryRelations
	}
	return CategoryOther
}

func EventTitle(eventType string) string {
	if title, ok := typeTitles[eventType]; ok {
		return title
	}
	if IsCustomFieldType(eventType) {
		if id := customFieldID(eventType); id != "" {
			return "Изменено поле #" + id
		}
		return "Изменено поле"
	}
	if strings.HasSuffix(eventType, "_unlinked") {
		return "Связь удалена"
	}
	if strings.HasSuffix(eventType, "_linked") {
		return "Объект связан"
	}
	if eventType == "" {
		return "Событие"
	}
	return "Событие " + eventType
}

func AuthorLabel(id int64, users []User) string {
	if id == 0 {
		return "Автор не указан"
	}
	for _, user := range users {
		if user.ID == id {
			if user.Name != "" {
				return user.Name
			}
			break
		}
	}
	return "Пользователь #" + strconv.FormatInt(id, 10)
}

func EntityTypeLabel(entityType string) string {
	switch entityType {
	case "lead", "leads":
		return "Сделка"
	case "contact", "contacts":
		return "Контакт"
	case "company", "companies":
		return "Компания"
	case "customer", "customers":
		return "Покупатель"
	case "task", "tasks":
		return "Задача"
	case "":
		return "Объект"
	default:
		return entityType
	}
}

func EntityLabel(entityType string, entityID int64, names []CatalogName) string {
	canonicalType := func(kind string) string {
		if plural, ok := CatalogEntityType(kind); ok {
			return plural
		}
		if kind == "task" {
			return "tasks"
		}
		return kind
	}
	for _, name := range names {
		if name.Kind == ObjectEntity && name.ID == entityID && canonicalType(name.EntityType) == canonicalType(entityType) && name.Name != "" {
			return name.Name
		}
	}
	if entityID == 0 && entityType == "" {
		return ""
	}
	if entityID == 0 {
		return EntityTypeLabel(entityType)
	}
	return EntityTypeLabel(entityType) + " #" + strconv.FormatInt(entityID, 10)
}

func HasPresentationQuery(q Query) bool {
	return len(q.Categories) > 0 || q.IncludeUnknownAuthors || q.Buckets != "" && q.Buckets != BucketNone || q.GroupID > 0
}

func ResolveBuckets(q Query) (string, error) {
	unit := q.Buckets
	if unit == "" || unit == BucketNone {
		return "", nil
	}
	if unit != BucketHour && unit != BucketDay && unit != BucketAuto {
		return "", Fail(InvalidArgument, "buckets must be hour, day, auto or none")
	}
	if q.Timezone == "" {
		return "", Fail(InvalidArgument, "timezone is required for time buckets")
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil {
		return "", Fail(InvalidArgument, "unknown account timezone")
	}
	if unit == BucketAuto {
		n, err := timelineBucketCount(q.From, q.To, BucketHour, q.Timezone)
		if err != nil {
			return "", err
		}
		unit = BucketHour
		if n > MaxHourBuckets {
			unit = BucketDay
		}
	}
	n, err := timelineBucketCount(q.From, q.To, unit, q.Timezone)
	if err != nil {
		return "", err
	}
	if unit == BucketHour && n > MaxHourBuckets {
		return "", Fail(InvalidArgument, "hour buckets exceed 48; use day or auto")
	}
	if unit == BucketDay && n > MaxDayBuckets {
		return "", Fail(InvalidArgument, "day buckets exceed the 31-day query bound")
	}
	return unit, nil
}

// QueryTimeBuckets is the single source of bucket boundaries for validation
// and owner aggregation. SQL receives these absolute instants, so converting a
// repeated local hour back to a timestamp cannot merge two different hours.
func QueryTimeBuckets(q Query) ([]TimeBucket, error) {
	unit, err := ResolveBuckets(q)
	if err != nil || unit == "" {
		return nil, err
	}
	return timelineBuckets(q.From, q.To, unit, q.Timezone)
}

func timelineBucketCount(from, to int64, unit, tz string) (int, error) {
	buckets, err := timelineBuckets(from, to, unit, tz)
	return len(buckets), err
}

func timelineBuckets(from, to int64, unit, tz string) ([]TimeBucket, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, Fail(InvalidArgument, "unknown account timezone")
	}
	start := time.Unix(from, 0).In(loc)
	if unit == BucketDay {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	} else {
		// Subtract within the actual instant; time.Date would choose an
		// arbitrary offset when this local hour occurs twice.
		start = start.Add(-time.Duration(start.Minute())*time.Minute - time.Duration(start.Second())*time.Second)
	}
	end := time.Unix(to, 0)
	var buckets []TimeBucket
	for t := start; !t.After(end); {
		next := t.Add(time.Hour)
		if unit == BucketDay {
			next = t.AddDate(0, 0, 1)
		}
		if !next.After(t) {
			return nil, Fail(InvalidArgument, "non-increasing calendar bucket boundary")
		}
		buckets = append(buckets, TimeBucket{StartAt: t.Unix(), EndAt: min(next.Unix()-1, to)})
		if len(buckets) > MaxHourBuckets {
			break
		}
		t = next
	}
	return buckets, nil
}

func SafeSQLToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func customFieldID(eventType string) string {
	rest := strings.TrimPrefix(eventType, "custom_field_")
	rest = strings.TrimSuffix(rest, "_value_changed")
	if rest == eventType || rest == "" || rest == "value" {
		return ""
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return rest
}

func validTimezone(s string) bool {
	if s == "" || len(s) > 64 {
		return s == ""
	}
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '/' || c == '+' || c == '-' {
			continue
		}
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}

var typeTitles = map[string]string{
	"task_added":                 "Задача создана",
	"task_completed":             "Задача завершена",
	"task_deleted":               "Задача удалена",
	"task_text_changed":          "Изменён текст задачи",
	"task_deadline_changed":      "Изменён срок задачи",
	"task_type_changed":          "Изменён тип задачи",
	"task_result_added":          "Добавлен результат задачи",
	"incoming_call":              "Входящий звонок",
	"outgoing_call":              "Исходящий звонок",
	"call_in":                    "Входящий звонок",
	"call_out":                   "Исходящий звонок",
	"common_note_added":          "Примечание добавлено",
	"common_note_deleted":        "Примечание удалено",
	"service_note_added":         "Служебное примечание",
	"incoming_chat_message":      "Входящее сообщение",
	"outgoing_chat_message":      "Исходящее сообщение",
	"entity_direct_message":      "Сообщение",
	"outgoing_sms":               "Исходящее SMS",
	"incoming_sms":               "Входящее SMS",
	"lead_status_changed":        "Изменён этап",
	"sale_field_changed":         "Изменён бюджет",
	"entity_responsible_changed": "Изменён ответственный",
	"attachment_note_added":      "Добавлено вложение",
	"lead_added":                 "Сделка создана",
	"contact_added":              "Контакт создан",
	"company_added":              "Компания создана",
	"custom_field_value_changed": "Изменено поле",
}
