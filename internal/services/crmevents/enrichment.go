package crmevents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type plannedObject struct {
	Kind, Key, ParentType, State, Reason, Source string
	ParentID, ObjectID                           int64
	Payload                                      json.RawMessage
}

type EnrichmentClaim struct {
	ID                            uuid.UUID
	InstallationID, IntegrationID uuid.UUID
	Kind, ParentType              string
	Objects                       []claimedObject
}

type claimedObject struct {
	Key, ParentType           string
	ParentID, ObjectID, Token int64
	Attempts                  int
}

type enrichmentSave struct {
	Key, State, Reason, Source, ParentType string
	ParentID                               int64
	Payload                                json.RawMessage
	FetchedAt                              time.Time
	Delay                                  time.Duration
	Task                                   *serviceapi.Task
}

func planEnrichment(event serviceapi.Event) []plannedObject {
	var out []plannedObject
	switch {
	case isTaskLifecycle(event.Type):
		out = appendTask(out, event.EntityID)
	case event.Type == "task_result_added":
		out = appendTask(out, event.EntityID)
		if note, ok := firstNote(event); ok && objectHas(note, "text") {
			out = append(out, readyNoteFromEvent(event, note))
		}
	case event.Type == "incoming_call" || event.Type == "outgoing_call":
		if note, ok := firstNote(event); ok {
			if objectHas(note, "duration") || objectHas(note, "link") || objectHas(note, "src") || objectHas(note, "source") {
				out = append(out, readyCallNoteFromEvent(event, note))
			} else {
				out = append(out, pendingNoteFromEvent(event, note))
			}
		}
		out = appendEntity(out, event.EntityType, event.EntityID)
	case isNoteEvent(event.Type) || event.Type == "outgoing_sms":
		if note, ok := firstNote(event); ok {
			if objectHas(note, "text") {
				out = append(out, readyNoteFromEvent(event, note))
			} else {
				out = append(out, pendingNoteFromEvent(event, note))
			}
		}
		out = appendEntity(out, event.EntityType, event.EntityID)
	case event.Type == "lead_status_changed":
		out = append(out, plannedObject{Kind: serviceapi.ObjectPipeline, Key: "leads", ParentType: "leads", State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded})
		out = appendEntity(out, event.EntityType, event.EntityID)
	case strings.HasPrefix(event.Type, "custom_field"):
		if et, ok := serviceapi.CatalogEntityType(event.EntityType); ok {
			out = append(out, plannedObject{Kind: serviceapi.ObjectCustomField, Key: et, ParentType: et, State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded})
		}
		out = appendEntity(out, event.EntityType, event.EntityID)
	case strings.HasSuffix(event.Type, "_linked") || strings.HasSuffix(event.Type, "_unlinked"):
		out = append(out, planLinkEntities(event)...)
		out = appendEntity(out, event.EntityType, event.EntityID)
	case isChatEvent(event.Type):
		out = appendEntity(out, event.EntityType, event.EntityID)
	default:
		out = appendEntity(out, event.EntityType, event.EntityID)
	}
	return dedupPlanned(out)
}

func isTaskLifecycle(t string) bool {
	switch t {
	case "task_added", "task_completed", "task_deleted":
		return true
	}
	return strings.HasPrefix(t, "task_") && strings.HasSuffix(t, "_changed")
}

func isNoteEvent(t string) bool {
	return strings.Contains(t, "_note_")
}

func isChatEvent(t string) bool {
	switch t {
	case "incoming_chat_message", "outgoing_chat_message", "entity_direct_message":
		return true
	}
	return false
}

func appendTask(out []plannedObject, id int64) []plannedObject {
	if id <= 0 {
		return out
	}
	return append(out, plannedObject{Kind: serviceapi.ObjectTask, Key: numericKey(id), ObjectID: id, State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded})
}

func appendEntity(out []plannedObject, entityType string, id int64) []plannedObject {
	et, ok := serviceapi.CatalogEntityType(entityType)
	if !ok || id <= 0 {
		return out
	}
	return append(out, plannedObject{Kind: serviceapi.ObjectEntity, Key: entityKey(et, id), ParentType: et, ParentID: id, ObjectID: id, State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded})
}

func planLinkEntities(event serviceapi.Event) []plannedObject {
	var out []plannedObject
	for _, key := range []string{"link", "unlink"} {
		for _, raw := range nestedObjectsFromEvent(event, key) {
			var wrap struct {
				Entity struct {
					Type string `json:"type"`
					ID   int64  `json:"id"`
				} `json:"entity"`
			}
			if json.Unmarshal(raw, &wrap) != nil {
				continue
			}
			out = appendEntity(out, wrap.Entity.Type, wrap.Entity.ID)
		}
	}
	return out
}

func pendingNoteFromEvent(event serviceapi.Event, note json.RawMessage) plannedObject {
	id := intField(note, "id")
	o := plannedObject{Kind: serviceapi.ObjectNote, Key: numericKey(id), ObjectID: id, State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded}
	if et, ok := serviceapi.CatalogEntityType(event.EntityType); ok && event.EntityID > 0 {
		o.ParentType = et
		o.ParentID = event.EntityID
		return o
	}
	o.State = serviceapi.EnrichmentUnavailable
	o.Reason = serviceapi.ReasonUnsupported
	return o
}

func readyNoteFromEvent(event serviceapi.Event, note json.RawMessage) plannedObject {
	id := intField(note, "id")
	et, _ := serviceapi.CatalogEntityType(event.EntityType)
	n := serviceapi.Note{ID: id, EntityID: event.EntityID, EntityType: et, CreatedBy: event.CreatedBy, Params: noteParams(note)}
	if t := stringField(note, "note_type"); t != "" {
		n.NoteType = t
	}
	extra := map[string]any{"current": false, "facts": NoteFacts(n)}
	if et != "" {
		extra["entity_type"] = et
	}
	if event.EntityID != 0 {
		extra["entity_id"] = event.EntityID
	}
	o := plannedObject{Kind: serviceapi.ObjectNote, Key: numericKey(id), ObjectID: id, ParentType: et, ParentID: event.EntityID, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceEventPayload, Payload: mergeJSON(note, extra)}
	return o
}

func readyCallNoteFromEvent(event serviceapi.Event, note json.RawMessage) plannedObject {
	id := intField(note, "id")
	et, _ := serviceapi.CatalogEntityType(event.EntityType)
	facts := CallFacts(event.Type, event.CreatedBy, note)
	if t := stringField(note, "note_type"); t != "" {
		if _, ok := facts["note_type"]; !ok {
			facts["note_type"] = t
		}
	}
	extra := map[string]any{"current": false, "facts": facts}
	if et != "" {
		extra["entity_type"] = et
	}
	if event.EntityID != 0 {
		extra["entity_id"] = event.EntityID
	}
	return plannedObject{Kind: serviceapi.ObjectNote, Key: numericKey(id), ObjectID: id, ParentType: et, ParentID: event.EntityID, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceEventPayload, Payload: mergeJSON(note, extra)}
}

func planNoteAfterTask(event serviceapi.Event, parentType string, parentID int64) (plannedObject, bool) {
	note, ok := firstNote(event)
	if !ok {
		return plannedObject{}, false
	}
	if objectHas(note, "text") {
		return plannedObject{}, false
	}
	et, ok := serviceapi.CatalogEntityType(parentType)
	if !ok || parentID <= 0 {
		return plannedObject{}, false
	}
	id := intField(note, "id")
	if id <= 0 {
		return plannedObject{}, false
	}
	return plannedObject{Kind: serviceapi.ObjectNote, Key: numericKey(id), ObjectID: id, ParentType: et, ParentID: parentID, State: serviceapi.EnrichmentPending, Reason: serviceapi.ReasonNotLoaded}, true
}

func firstNote(event serviceapi.Event) (json.RawMessage, bool) {
	notes := nestedObjectsFromEvent(event, "note")
	if len(notes) == 0 {
		return nil, false
	}
	if intField(notes[0], "id") <= 0 {
		return nil, false
	}
	return notes[0], true
}

func nestedObjectsFromEvent(event serviceapi.Event, key string) []json.RawMessage {
	var out []json.RawMessage
	out = append(out, nestedObjects(event.ValueBefore, key)...)
	out = append(out, nestedObjects(event.ValueAfter, key)...)
	return out
}

func nestedObjects(raw json.RawMessage, key string) []json.RawMessage {
	var out []json.RawMessage
	for _, entry := range changeEntries(raw) {
		obj := jsonObject(entry)
		if v, ok := obj[key]; ok {
			out = append(out, v)
		}
	}
	return out
}

func changeEntries(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	return arr
}

func objectHas(raw json.RawMessage, field string) bool {
	_, ok := jsonObject(raw)[field]
	return ok
}

func intField(raw json.RawMessage, field string) int64 {
	n, _ := jsonInt(jsonObject(raw)[field])
	return n
}

func stringField(raw json.RawMessage, field string) string {
	v, _ := jsonValue(jsonObject(raw)[field]).(string)
	return v
}

func noteParams(note json.RawMessage) json.RawMessage {
	if raw, ok := jsonObject(note)["params"]; ok {
		return raw
	}
	return note
}

func numericKey(id int64) string { return strconv.FormatInt(id, 10) }

func entityKey(entityType string, id int64) string {
	return entityType + ":" + numericKey(id)
}

func parseEntityKey(key string) (string, int64, bool) {
	kind, rest, ok := strings.Cut(key, ":")
	if !ok {
		return "", 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return kind, id, err == nil && id > 0 && kind != ""
}

func dedupPlanned(in []plannedObject) []plannedObject {
	seen := map[string]int{}
	out := make([]plannedObject, 0, len(in))
	for _, o := range in {
		if o.Key == "" || o.Kind == "" {
			continue
		}
		k := o.Kind + "\x00" + o.Key
		if i, ok := seen[k]; ok {
			if o.State == serviceapi.EnrichmentReady && out[i].State != serviceapi.EnrichmentReady {
				out[i] = o
			}
			continue
		}
		seen[k] = len(out)
		out = append(out, o)
	}
	return out
}

func mergeJSON(base json.RawMessage, extra map[string]any) json.RawMessage {
	if len(base) == 0 || string(base) == "null" {
		base = []byte(`{}`)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(base, &obj) != nil || obj == nil {
		obj = map[string]json.RawMessage{}
	}
	for k, v := range extra {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		obj[k] = b
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

func objectPayload(base any, facts map[string]any, current bool) json.RawMessage {
	b, err := json.Marshal(base)
	if err != nil {
		b = []byte(`{}`)
	}
	extra := map[string]any{"current": current}
	if len(facts) > 0 {
		extra["facts"] = facts
	}
	return mergeJSON(b, extra)
}

func payloadHash(p json.RawMessage) []byte {
	if len(p) == 0 {
		return nil
	}
	sum := sha256.Sum256(p)
	return sum[:]
}

func enrichmentGrant(kind string) string {
	switch kind {
	case serviceapi.ObjectNote:
		return serviceapi.ActionNotes
	case serviceapi.ObjectTask:
		return serviceapi.ActionTasks
	case serviceapi.ObjectPipeline:
		return serviceapi.ActionPipelines
	case serviceapi.ObjectCustomField:
		return serviceapi.ActionCustomFields
	case serviceapi.ObjectEntity:
		return serviceapi.ActionEntities
	default:
		return ""
	}
}

func enrichmentFailure(err error, attempts, maxAttempts int) (state, reason string, delay time.Duration) {
	code := serviceapi.ErrorCode(err)
	backoff := time.Duration(1<<min(attempts, 8)) * time.Second
	if backoff > 10*time.Minute {
		backoff = 10 * time.Minute
	}
	switch code {
	case serviceapi.NotFound:
		return serviceapi.EnrichmentUnavailable, serviceapi.ReasonNotFound, 15 * time.Minute
	case serviceapi.PermissionDenied, serviceapi.Unauthenticated:
		return serviceapi.EnrichmentUnavailable, serviceapi.ReasonPermissionDenied, time.Hour
	case serviceapi.ReauthRequired:
		return serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, time.Hour
	case serviceapi.InvalidArgument:
		return serviceapi.EnrichmentUnavailable, serviceapi.ReasonInvalid, 0
	case serviceapi.ResourceExhausted, serviceapi.Unavailable, serviceapi.DeadlineExceeded:
		if attempts >= maxAttempts {
			return serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, 10 * time.Minute
		}
		return serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, backoff
	default:
		if attempts >= maxAttempts {
			return serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, 10 * time.Minute
		}
		return serviceapi.EnrichmentRetry, serviceapi.ReasonTemporary, backoff
	}
}

// Stored objects describe current upstream state. Historical facts are projected
// from this event, never shared through a note ID with other events.
func eventPayloadObjects(event serviceapi.Event) []serviceapi.EnrichmentObject {
	var objects []serviceapi.EnrichmentObject
	for _, o := range planEnrichment(event) {
		if o.Source == serviceapi.SourceEventPayload {
			objects = append(objects, serviceapi.EnrichmentObject{
				ObjectKind: o.Kind, ObjectKey: o.Key, State: o.State,
				ReasonCode: o.Reason, Source: o.Source, Payload: o.Payload,
			})
		}
	}
	return objects
}

// The upstream limit applies to individual note params/text, not an entire
// catalog or the object envelope plus derived facts. Keep the complete object
// within a separate owner limit, below the detail response limit of 3 MiB.
const maxEnrichmentObjectBytes = 1 << 20

func enrichmentTTL(kind string) time.Duration {
	switch kind {
	case serviceapi.ObjectPipeline, serviceapi.ObjectCustomField, serviceapi.ObjectEntity:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

func attachSidecar(event serviceapi.Event, objects []serviceapi.EnrichmentObject) serviceapi.Event {
	if len(objects) == 0 {
		event.Enrichment = nil
		event.Names = nil
		return event
	}
	event.Enrichment = objects
	event.Names = catalogNames(event, objects)
	if len(event.Names) == 0 {
		event.Names = nil
	}
	return event
}

func catalogNames(event serviceapi.Event, objects []serviceapi.EnrichmentObject) []serviceapi.CatalogName {
	var names []serviceapi.CatalogName
	statusIDs := leadStatusIDs(event)
	fieldIDs := customFieldIDs(event)
	for _, o := range objects {
		switch o.ObjectKind {
		case serviceapi.ObjectEntity:
			if o.State != serviceapi.EnrichmentReady && o.State != serviceapi.EnrichmentUnavailable {
				continue
			}
			var ent serviceapi.EntityName
			_ = json.Unmarshal(o.Payload, &ent)
			if ent.ID == 0 {
				if et, id, ok := parseEntityKey(o.ObjectKey); ok {
					ent.EntityType, ent.ID = et, id
				}
			}
			names = append(names, serviceapi.CatalogName{Current: true, Kind: serviceapi.ObjectEntity, Name: ent.Name, State: o.State, EntityType: ent.EntityType, ID: ent.ID})
		case serviceapi.ObjectPipeline:
			if o.State != serviceapi.EnrichmentReady {
				continue
			}
			var cat serviceapi.PipelineCatalog
			_ = json.Unmarshal(o.Payload, &cat)
			byID := map[int64]serviceapi.PipelineStatus{}
			for _, p := range cat.Pipelines {
				for _, st := range p.Statuses {
					byID[st.ID] = st
				}
			}
			for _, id := range statusIDs {
				st, ok := byID[id]
				state := serviceapi.EnrichmentReady
				if !ok || st.Name == "" {
					state = serviceapi.EnrichmentUnavailable
				}
				names = append(names, serviceapi.CatalogName{Current: true, Kind: "pipeline_status", Name: st.Name, State: state, EntityType: "leads", ID: id})
			}
		case serviceapi.ObjectCustomField:
			if o.State != serviceapi.EnrichmentReady {
				continue
			}
			var cat serviceapi.CustomFieldCatalog
			_ = json.Unmarshal(o.Payload, &cat)
			byID := map[int64]serviceapi.CustomField{}
			for _, f := range cat.Fields {
				byID[f.ID] = f
			}
			et := o.ObjectKey
			for _, id := range fieldIDs {
				f, ok := byID[id]
				state := serviceapi.EnrichmentReady
				if !ok {
					state = serviceapi.EnrichmentUnavailable
				}
				if f.EntityType != "" {
					et = f.EntityType
				}
				names = append(names, serviceapi.CatalogName{Current: true, Kind: serviceapi.ObjectCustomField, Name: f.Name, State: state, EntityType: et, ID: id})
			}
		}
	}
	return names
}

func leadStatusIDs(event serviceapi.Event) []int64 {
	var ids []int64
	seen := map[int64]bool{}
	for _, raw := range nestedObjectsFromEvent(event, "lead_status") {
		id := intField(raw, "id")
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func customFieldIDs(event serviceapi.Event) []int64 {
	var ids []int64
	seen := map[int64]bool{}
	for _, raw := range nestedObjectsFromEvent(event, "custom_field_value") {
		id := intField(raw, "field_id")
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

func enrichmentCurrent(state, source string, payload json.RawMessage) bool {
	var wrap struct {
		Current bool `json:"current"`
	}
	if json.Unmarshal(payload, &wrap) == nil && wrap.Current {
		return true
	}
	return state == serviceapi.EnrichmentReady && source != "" && source != serviceapi.SourceEventPayload
}

func claimedIDs(c EnrichmentClaim) []int64 {
	ids := make([]int64, 0, len(c.Objects))
	seen := map[int64]bool{}
	for _, o := range c.Objects {
		if o.ObjectID <= 0 || seen[o.ObjectID] {
			continue
		}
		seen[o.ObjectID] = true
		ids = append(ids, o.ObjectID)
	}
	return ids
}

func unavailableSaves(c EnrichmentClaim, reason, source string, delay time.Duration) []enrichmentSave {
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: reason, Source: source, ParentType: o.ParentType, ParentID: o.ParentID, Delay: delay})
	}
	return out
}

func noteLooksLikeCall(note serviceapi.Note) bool {
	if strings.Contains(note.NoteType, "call") {
		return true
	}
	fields := jsonObject(note.Params)
	for _, key := range []string{"duration", "link", "src", "source", "uniq", "phone", "call_responsible"} {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

func (s *Service) fetchEnrichment(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	if s.gateway == nil {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "gateway unavailable")
	}
	switch c.Kind {
	case serviceapi.ObjectNote:
		return s.fetchNotes(ctx, auth, c)
	case serviceapi.ObjectTask:
		return s.fetchTasks(ctx, auth, c)
	case serviceapi.ObjectPipeline:
		return s.fetchPipelines(ctx, auth, c)
	case serviceapi.ObjectCustomField:
		return s.fetchCustomFields(ctx, auth, c)
	case serviceapi.ObjectEntity:
		return s.fetchEntities(ctx, auth, c)
	default:
		return unavailableSaves(c, serviceapi.ReasonUnsupported, "", 0), nil
	}
}

func (s *Service) fetchNotes(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	et, ok := serviceapi.CatalogEntityType(c.ParentType)
	if !ok {
		return unavailableSaves(c, serviceapi.ReasonUnsupported, serviceapi.SourceNotesAPI, 0), nil
	}
	ids := claimedIDs(c)
	if err := serviceapi.ValidateIDBatch(ids, serviceapi.EnrichmentBatchLimit); err != nil {
		return unavailableSaves(c, serviceapi.ReasonInvalid, serviceapi.SourceNotesAPI, 0), nil
	}
	page, err := s.gateway.Notes(ctx, serviceapi.NotesRequest{Auth: auth, EntityType: et, IDs: ids})
	if err != nil {
		return nil, err
	}
	byID := map[int64]serviceapi.Note{}
	for _, n := range page.Notes {
		byID[n.ID] = n
	}
	now := s.cfg.Now()
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		if slices.Contains(page.InvalidIDs, o.ObjectID) {
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonInvalid, Source: serviceapi.SourceNotesAPI, ParentType: o.ParentType, ParentID: o.ParentID})
			continue
		}
		n, ok := byID[o.ObjectID]
		if !ok {
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonNotFound, Source: serviceapi.SourceNotesAPI, ParentType: o.ParentType, ParentID: o.ParentID, Delay: 15 * time.Minute})
			continue
		}
		if n.EntityType == "" {
			n.EntityType = et
		}
		facts := NoteFacts(n)
		if noteLooksLikeCall(n) {
			for k, v := range CallFacts(n.NoteType, n.CreatedBy, n.Params) {
				facts[k] = v
			}
		}
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceNotesAPI, ParentType: n.EntityType, ParentID: n.EntityID, Payload: objectPayload(n, facts, true), FetchedAt: now})
	}
	return out, nil
}

func (s *Service) fetchTasks(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	ids := claimedIDs(c)
	if err := serviceapi.ValidateIDBatch(ids, serviceapi.EnrichmentBatchLimit); err != nil {
		return unavailableSaves(c, serviceapi.ReasonInvalid, serviceapi.SourceTasksAPI, 0), nil
	}
	page, err := s.gateway.Tasks(ctx, serviceapi.TasksRequest{Auth: auth, IDs: ids})
	if err != nil {
		return nil, err
	}
	byID := map[int64]serviceapi.Task{}
	for _, task := range page.Tasks {
		byID[task.ID] = task
	}
	now := s.cfg.Now()
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		if slices.Contains(page.InvalidIDs, o.ObjectID) {
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonInvalid, Source: serviceapi.SourceTasksAPI, ParentType: o.ParentType, ParentID: o.ParentID})
			continue
		}
		task, ok := byID[o.ObjectID]
		if !ok {
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonNotFound, Source: serviceapi.SourceTasksAPI, Delay: 15 * time.Minute})
			continue
		}
		copy := task
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceTasksAPI, ParentType: task.EntityType, ParentID: task.EntityID, Payload: objectPayload(task, TaskFacts(task), true), FetchedAt: now, Task: &copy})
	}
	return out, nil
}

func (s *Service) fetchPipelines(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	cat, err := s.gateway.Pipelines(ctx, serviceapi.CatalogRequest{Auth: auth})
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	payload := objectPayload(cat, nil, true)
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourcePipelinesAPI, ParentType: o.ParentType, Payload: payload, FetchedAt: now})
	}
	return out, nil
}

func (s *Service) fetchCustomFields(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	et, ok := serviceapi.CatalogEntityType(c.ParentType)
	if !ok {
		et = c.ParentType
	}
	if et == "" {
		return unavailableSaves(c, serviceapi.ReasonUnsupported, serviceapi.SourceCustomFieldsAPI, 0), nil
	}
	cat, err := s.gateway.CustomFields(ctx, serviceapi.CustomFieldsRequest{Auth: auth, EntityType: et})
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	payload := objectPayload(cat, nil, true)
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceCustomFieldsAPI, ParentType: et, Payload: payload, FetchedAt: now})
	}
	return out, nil
}

func (s *Service) fetchEntities(ctx context.Context, auth serviceapi.Auth, c EnrichmentClaim) ([]enrichmentSave, error) {
	et, ok := serviceapi.CatalogEntityType(c.ParentType)
	if !ok {
		return unavailableSaves(c, serviceapi.ReasonUnsupported, serviceapi.SourceEntitiesAPI, 0), nil
	}
	ids := claimedIDs(c)
	if err := serviceapi.ValidateIDBatch(ids, serviceapi.EnrichmentBatchLimit); err != nil {
		return unavailableSaves(c, serviceapi.ReasonInvalid, serviceapi.SourceEntitiesAPI, 0), nil
	}
	cat, err := s.gateway.Entities(ctx, serviceapi.EntitiesRequest{Auth: auth, EntityType: et, IDs: ids})
	if err != nil {
		return nil, err
	}
	byID := map[int64]serviceapi.EntityName{}
	for _, e := range cat.Entities {
		byID[e.ID] = e
	}
	now := s.cfg.Now()
	out := make([]enrichmentSave, 0, len(c.Objects))
	for _, o := range c.Objects {
		if slices.Contains(cat.InvalidIDs, o.ObjectID) {
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonInvalid, Source: serviceapi.SourceEntitiesAPI, ParentType: o.ParentType, ParentID: o.ParentID})
			continue
		}
		ent, ok := byID[o.ObjectID]
		if !ok {
			ent = serviceapi.EntityName{ID: o.ObjectID, EntityType: et}
			out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentUnavailable, Reason: serviceapi.ReasonNotFound, Source: serviceapi.SourceEntitiesAPI, ParentType: et, ParentID: o.ObjectID, Payload: objectPayload(ent, nil, true), Delay: 15 * time.Minute})
			continue
		}
		if ent.EntityType == "" {
			ent.EntityType = et
		}
		out = append(out, enrichmentSave{Key: o.Key, State: serviceapi.EnrichmentReady, Source: serviceapi.SourceEntitiesAPI, ParentType: ent.EntityType, ParentID: ent.ID, Payload: objectPayload(ent, nil, true), FetchedAt: now})
	}
	return out, nil
}
