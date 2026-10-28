package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type employeeTaskAPI interface {
	ListOpenTasksPage(context.Context, uuid.UUID, int64, int) (amocrm.OpenTaskPage, error)
	HasOpenTask(context.Context, uuid.UUID, int64, string, int64) (bool, error)
	CompleteTask(context.Context, uuid.UUID, int64, string) error
	ListTaskTypes(context.Context, uuid.UUID) ([]amocrm.TaskType, error)
	GetEntityContext(context.Context, uuid.UUID, string, int64) (amocrm.EntityContext, error)
	LeadParticipants(context.Context, uuid.UUID, int64) ([]int64, []int64, error)
	ParticipantLeads(context.Context, uuid.UUID, string, int64) (string, []int64, error)
}

const (
	// Twenty pages of 250 match the previous backend's queue ceiling.
	taskQueuePages     = 20
	taskQueueTextLimit = 1024
	taskQueueNameLimit = 255
	// Time left to publish the snapshot after paging and names.
	taskQueuePublishReserve = 3 * time.Second
	taskQueuePayloadLimit   = 1 << 20
	taskQueueResponseCap    = 2 << 20
	entityNameTTL           = 10 * time.Minute
	entityNameCacheLimit    = 50000
	relatedTTL              = time.Minute
	relatedCacheLimit       = 4096
	relatedParticipants     = 10
	// A per-actor lead verdict spares the open lead's checks on every poll;
	// it never outlives one queue TTL.
	visibilityTTL        = TaskQueueTTL
	visibilityCacheLimit = 16384
)

type cachedTaskTypes struct {
	names map[int64]string
	until time.Time
}
type entityKey struct {
	installation uuid.UUID
	entityType   string
	id           int64
}
type cachedName struct {
	name  string
	until time.Time
}
type relatedKey struct {
	installation uuid.UUID
	lead         int64
}
type cachedRelated struct {
	labels map[string]string
	until  time.Time
}
type visibilityKey struct {
	installation uuid.UUID
	actor, lead  int64
}
type cachedVisibility struct {
	visible bool
	until   time.Time
}

var entityLabels = map[string]string{"leads": "Сделка", "contacts": "Контакт", "companies": "Компания", "customers": "Покупатель"}

func (s *Service) employee(ctx context.Context, auth serviceapi.Auth, action string) (serviceapi.Principal, employeeTaskAPI, error) {
	p, err := s.policy.Validate(ctx, auth, serviceapi.GatewayService, action)
	if err != nil {
		return serviceapi.Principal{}, nil, err
	}
	if p.Kind != serviceapi.PrincipalKindEmployee || p.ActorID <= 0 {
		return serviceapi.Principal{}, nil, serviceapi.Fail(serviceapi.PermissionDenied, "employee identity required")
	}
	api, ok := s.api.(employeeTaskAPI)
	if !ok || s.taskQueues == nil {
		return serviceapi.Principal{}, nil, serviceapi.Fail(serviceapi.Unavailable, "employee task access unavailable")
	}
	return p, api, nil
}

// OwnTasks serves the employee's shared queue snapshot. The responsible user
// is the verified actor; the browser never names whose tasks are read.
func (s *Service) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	if r.LeadID < 0 {
		return serviceapi.TaskQueue{}, serviceapi.Fail(serviceapi.InvalidArgument, "lead_id must be positive")
	}
	p, api, err := s.employee(ctx, r.Auth, serviceapi.ActionOwnTasks)
	if err != nil {
		return serviceapi.TaskQueue{}, err
	}
	snap, err := s.taskQueues.Load(ctx, p.Scope, p.ActorID, func(ctx context.Context) ([]storedTask, bool, error) {
		return s.loadOpenTasks(ctx, api, p.InstallationID, p.ActorID)
	})
	if err != nil {
		return serviceapi.TaskQueue{}, err
	}
	out := serviceapi.TaskQueue{Tasks: make([]serviceapi.QueueTask, 0, len(snap.Tasks)), Complete: snap.Complete, Stale: snap.Stale, ErrorCode: snap.ErrorCode, FetchedAt: snap.FetchedAt.Unix(), FreshUntil: snap.FreshUntil.Unix()}
	if snap.RetryAfter > 0 {
		out.RetryAfterSeconds = int64((snap.RetryAfter + time.Second - 1) / time.Second)
	}
	if snap.FetchedAt.IsZero() {
		out.FetchedAt = 0
	}
	// Decorations are best effort: a busy budget leaves names empty, never
	// hides the employee's work. Card names come from the snapshot only.
	types := s.taskTypeNames(ctx, api, p.InstallationID)
	// Participants of a lead the employee cannot see are never disclosed;
	// the shared labels cache is read only after this per-actor check, and
	// never for a stale snapshot whose tasks may no longer be the employee's.
	var related map[string]string
	if r.LeadID > 0 && !snap.Stale && s.leadAccessible(ctx, api, p, r.LeadID) {
		related = s.relatedLabels(ctx, api, p.InstallationID, r.LeadID)
	}
	size := 0
	for _, task := range snap.Tasks {
		key := task.EntityType + ":" + strconv.FormatInt(task.EntityID, 10)
		item := serviceapi.QueueTask{ID: task.ID, EntityID: task.EntityID, EntityType: task.EntityType, EntityName: task.EntityName, Text: task.Text, CompleteTill: task.CompleteTill, TaskTypeID: task.TaskTypeID, TaskTypeName: types[task.TaskTypeID], Related: related[key], UpdatedAt: task.UpdatedAt}
		encoded, _ := json.Marshal(item)
		if size += len(encoded) + 1; size > taskQueueResponseCap {
			out.Complete = false
			break
		}
		out.Tasks = append(out.Tasks, item)
	}
	return out, nil
}

// loadOpenTasks captures card names right after the live queue read, which
// proves the employee's access to those cards; a snapshot served later, stale
// included, never looks a card up again. Names are best effort.
func (s *Service) loadOpenTasks(ctx context.Context, api employeeTaskAPI, installation uuid.UUID, actor int64) ([]storedTask, bool, error) {
	tasks, complete, err := fetchOpenTasks(ctx, api, installation, actor)
	if err != nil {
		return nil, false, err
	}
	// Names are best effort: a slow budget must not cost the snapshot write.
	nameCtx, cancel := ctx, context.CancelFunc(func() {})
	if deadline, ok := ctx.Deadline(); ok {
		nameCtx, cancel = context.WithDeadline(ctx, deadline.Add(-taskQueuePublishReserve))
	}
	defer cancel()
	names := s.names(nameCtx, installation, tasks)
	size := 0
	for i := range tasks {
		tasks[i].EntityName = truncateText(names[tasks[i].EntityType+":"+strconv.FormatInt(tasks[i].EntityID, 10)], taskQueueNameLimit)
		encoded, _ := json.Marshal(tasks[i])
		if size += len(encoded) + 1; size > taskQueuePayloadLimit {
			return tasks[:i], false, nil
		}
	}
	return tasks, complete, nil
}

func fetchOpenTasks(ctx context.Context, api employeeTaskAPI, installation uuid.UUID, actor int64) ([]storedTask, bool, error) {
	tasks := []storedTask{}
	seen := map[int64]bool{}
	size := 0
	for page := 1; page <= taskQueuePages; page++ {
		result, err := api.ListOpenTasksPage(ctx, installation, actor, page)
		if err != nil {
			return nil, false, corepolicy.MapUpstreamError(err)
		}
		for _, task := range result.Tasks {
			// Offset pages can shift while tasks are completed; keep one copy.
			if seen[task.ID] {
				continue
			}
			seen[task.ID] = true
			item := storedTask{ID: task.ID, EntityID: task.EntityID, EntityType: task.EntityType, Text: truncateText(task.Text, taskQueueTextLimit), CompleteTill: task.CompleteTill, TaskTypeID: task.TaskTypeID, UpdatedAt: task.UpdatedAt}
			encoded, _ := json.Marshal(item)
			if size += len(encoded) + 1; size > taskQueuePayloadLimit {
				return tasks, false, nil
			}
			tasks = append(tasks, item)
		}
		if !result.HasNext {
			return tasks, true, nil
		}
	}
	return tasks, false, nil
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

// OwnTask reads one task live. Context adds the attached card; the employee
// is responsible for the task, so its card is within the task's visibility.
func (s *Service) OwnTask(ctx context.Context, r serviceapi.OwnTaskRequest) (serviceapi.TaskDetail, error) {
	if err := serviceapi.ValidateTaskID(r.TaskID); err != nil {
		return serviceapi.TaskDetail{}, err
	}
	p, api, err := s.employee(ctx, r.Auth, serviceapi.ActionOwnTasks)
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	detail, err := s.ownTask(ctx, api, p, r.TaskID)
	if err != nil || !r.WithContext {
		return detail, err
	}
	if _, ok := entityLabels[detail.EntityType]; ok && detail.EntityID > 0 {
		entity, err := api.GetEntityContext(ctx, p.InstallationID, detail.EntityType, detail.EntityID)
		if err == nil {
			detail.Entity = s.describe(ctx, p.InstallationID, entity)
		}
	}
	return detail, nil
}

func (s *Service) ownTask(ctx context.Context, api employeeTaskAPI, p serviceapi.Principal, id int64) (serviceapi.TaskDetail, error) {
	tasks, err := s.api.ListTasks(ctx, p.InstallationID, []int64{id})
	if err != nil {
		return serviceapi.TaskDetail{}, corepolicy.MapUpstreamError(err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		return serviceapi.TaskDetail{}, serviceapi.Fail(serviceapi.NotFound, "task not found")
	}
	task := tasks[0]
	// A rejected payload carries no owner, so it cannot be attributed.
	if task.Invalid {
		return serviceapi.TaskDetail{}, serviceapi.Fail(serviceapi.ResourceExhausted, "task exceeds supported size")
	}
	// Another employee's task is reported as denied before any of its fields.
	if task.ResponsibleUserID != p.ActorID {
		return serviceapi.TaskDetail{}, serviceapi.Fail(serviceapi.PermissionDenied, "task belongs to another employee")
	}
	detail := serviceapi.TaskDetail{ID: task.ID, EntityID: task.EntityID, EntityType: task.EntityType, ResponsibleUserID: task.ResponsibleUserID, Text: task.Text, CompleteTill: task.CompleteTill, TaskTypeID: task.TaskTypeID, IsCompleted: task.IsCompleted, ResultText: task.ResultText, UpdatedAt: task.UpdatedAt}
	detail.TaskTypeName = s.taskTypeNames(ctx, api, p.InstallationID)[task.TaskTypeID]
	return detail, nil
}

// CompleteOwnTask is lease/GET/compare/PATCH: one holder per task across
// tabs and replicas, and a repeated command after a lost response finds the
// task completed and does not write again.
func (s *Service) CompleteOwnTask(ctx context.Context, r serviceapi.CompleteTaskRequest) (serviceapi.TaskDetail, error) {
	if err := serviceapi.ValidateTaskID(r.TaskID); err != nil {
		return serviceapi.TaskDetail{}, err
	}
	if err := serviceapi.ValidateTaskResult(r.Result); err != nil {
		return serviceapi.TaskDetail{}, err
	}
	p, api, err := s.employee(ctx, r.Auth, serviceapi.ActionCompleteTask)
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	token, err := s.taskQueues.AcquireCompletion(ctx, p.InstallationID, r.TaskID)
	if errors.Is(err, errCompletionBusy) {
		// Another tab or replica is completing it: report its outcome.
		detail, err := s.ownTask(ctx, api, p, r.TaskID)
		if err != nil || detail.IsCompleted {
			return detail, err
		}
		return serviceapi.TaskDetail{}, &serviceapi.Error{Code: serviceapi.Unavailable, Message: "task completion is in progress", RetryAfter: time.Second}
	}
	if err != nil {
		return serviceapi.TaskDetail{}, err
	}
	defer s.taskQueues.ReleaseCompletion(ctx, p.InstallationID, r.TaskID, token)
	workCtx, cancelWork := context.WithTimeout(ctx, taskCompletionWork)
	defer cancelWork()
	detail, err := s.ownTask(workCtx, api, p, r.TaskID)
	if err != nil || detail.IsCompleted {
		return detail, err
	}
	err = api.CompleteTask(workCtx, p.InstallationID, r.TaskID, r.Result)
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskCompletionReconcile)
	defer cancel()
	if err != nil {
		// The PATCH may have reached amoCRM: amoCRM's state decides.
		if completionUncertain(err) {
			if done, readErr := s.ownTask(storeCtx, api, p, r.TaskID); readErr == nil && done.IsCompleted {
				s.dropCompleted(storeCtx, p, r.TaskID)
				return done, nil
			}
		}
		_ = s.taskQueues.Invalidate(storeCtx, p.Scope, p.ActorID)
		return serviceapi.TaskDetail{}, corepolicy.MapUpstreamError(err)
	}
	s.dropCompleted(storeCtx, p, r.TaskID)
	detail.IsCompleted, detail.ResultText = true, r.Result
	return detail, nil
}

func (s *Service) dropCompleted(ctx context.Context, p serviceapi.Principal, taskID int64) {
	if err := s.taskQueues.RemoveTask(ctx, p.Scope, p.ActorID, taskID); err != nil {
		_ = s.taskQueues.Invalidate(ctx, p.Scope, p.ActorID)
	}
}

// completionUncertain is false only for amoCRM's definitive refusals and
// local shedding; a timeout, transport error or 5xx may have applied.
func completionUncertain(err error) bool {
	var api *amocrm.APIError
	if errors.As(err, &api) {
		return api.Kind == amocrm.ErrorTemporary || api.Kind == amocrm.ErrorPermanent
	}
	return true
}

// OwnLead decorates an Imbox interval with its lead. The server account can
// read any card, so names are returned only for leads the employee can see.
func (s *Service) OwnLead(ctx context.Context, r serviceapi.OwnLeadRequest) (serviceapi.EntityContext, error) {
	if r.LeadID <= 0 {
		return serviceapi.EntityContext{}, serviceapi.Fail(serviceapi.InvalidArgument, "lead_id must be positive")
	}
	p, api, err := s.employee(ctx, r.Auth, serviceapi.ActionOwnTasks)
	if err != nil {
		return serviceapi.EntityContext{}, err
	}
	lead, visible, err := s.leadVisible(ctx, api, p, r.LeadID)
	if err != nil {
		return serviceapi.EntityContext{}, corepolicy.MapUpstreamError(err)
	}
	if !visible {
		return serviceapi.EntityContext{ID: r.LeadID, EntityType: "leads", Restricted: true}, nil
	}
	return s.describe(ctx, p.InstallationID, lead), nil
}

// leadVisible applies the visibility rule live: the employee is responsible
// for the lead or for an open task on it. A queue snapshot proves nothing
// after reassignment, and a failed check never grants access.
func (s *Service) leadVisible(ctx context.Context, api employeeTaskAPI, p serviceapi.Principal, lead int64) (amocrm.EntityContext, bool, error) {
	entity, err := api.GetEntityContext(ctx, p.InstallationID, "leads", lead)
	if err != nil {
		return amocrm.EntityContext{}, false, err
	}
	visible := entity.ResponsibleUserID == p.ActorID
	if !visible {
		if visible, err = api.HasOpenTask(ctx, p.InstallationID, p.ActorID, "leads", lead); err != nil {
			return amocrm.EntityContext{}, false, err
		}
	}
	s.rememberVisibility(visibilityKey{p.InstallationID, p.ActorID, lead}, visible)
	return entity, visible, nil
}

// leadAccessible serves a verdict at most visibilityTTL old, else checks live.
func (s *Service) leadAccessible(ctx context.Context, api employeeTaskAPI, p serviceapi.Principal, lead int64) bool {
	s.mu.Lock()
	entry, ok := s.visibility[visibilityKey{p.InstallationID, p.ActorID, lead}]
	s.mu.Unlock()
	if ok && time.Now().Before(entry.until) {
		return entry.visible
	}
	_, visible, err := s.leadVisible(ctx, api, p, lead)
	return err == nil && visible
}

func (s *Service) rememberVisibility(key visibilityKey, visible bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.visibility) >= visibilityCacheLimit {
		for k, v := range s.visibility {
			if now.After(v.until) {
				delete(s.visibility, k)
			}
		}
	}
	if len(s.visibility) < visibilityCacheLimit {
		s.visibility[key] = cachedVisibility{visible: visible, until: now.Add(visibilityTTL)}
	}
}

func (s *Service) describe(ctx context.Context, installation uuid.UUID, entity amocrm.EntityContext) serviceapi.EntityContext {
	out := serviceapi.EntityContext{ID: entity.ID, EntityType: entity.EntityType, Name: entity.Name, PipelineID: entity.PipelineID, StatusID: entity.StatusID}
	if entity.PipelineID <= 0 {
		return out
	}
	pipelines, err := s.pipelineEntry(ctx, installation)
	if err != nil {
		return out
	}
	for _, pipe := range pipelines.data {
		if pipe.ID != entity.PipelineID {
			continue
		}
		out.PipelineName = pipe.Name
		for _, status := range pipe.Statuses {
			if status.ID == entity.StatusID {
				out.StatusName, out.StatusColor = status.Name, status.Color
			}
		}
	}
	return out
}

func (s *Service) taskTypeNames(ctx context.Context, api employeeTaskAPI, installation uuid.UUID) map[int64]string {
	s.mu.Lock()
	entry, ok := s.taskTypes[installation]
	s.mu.Unlock()
	if ok && !time.Now().After(entry.until) {
		return entry.names
	}
	types, err := api.ListTaskTypes(ctx, installation)
	if err != nil {
		return entry.names
	}
	entry = cachedTaskTypes{names: make(map[int64]string, len(types)), until: time.Now().Add(catalogTTL)}
	for _, t := range types {
		entry.names[t.ID] = t.Name
	}
	s.mu.Lock()
	if len(s.taskTypes) >= 256 {
		now := time.Now()
		for id, v := range s.taskTypes {
			if now.After(v.until) {
				delete(s.taskTypes, id)
			}
		}
	}
	if len(s.taskTypes) < 256 {
		s.taskTypes[installation] = entry
	}
	s.mu.Unlock()
	return entry.names
}

// names resolves card names in batches; unknown or deleted cards are cached
// empty so that every refresh does not ask amoCRM again.
func (s *Service) names(ctx context.Context, installation uuid.UUID, tasks []storedTask) map[string]string {
	out := map[string]string{}
	missing := map[string][]int64{}
	now := time.Now()
	s.mu.Lock()
	for _, task := range tasks {
		if _, ok := entityLabels[task.EntityType]; !ok || task.EntityID <= 0 {
			continue
		}
		key := task.EntityType + ":" + strconv.FormatInt(task.EntityID, 10)
		if _, done := out[key]; done {
			continue
		}
		if cached, ok := s.entityNames[entityKey{installation, task.EntityType, task.EntityID}]; ok && now.Before(cached.until) {
			out[key] = cached.name
			continue
		}
		out[key] = ""
		missing[task.EntityType] = append(missing[task.EntityType], task.EntityID)
	}
	s.mu.Unlock()
	for entityType, ids := range missing {
		for i := 0; i < len(ids); i += serviceapi.EnrichmentBatchLimit {
			batch := ids[i:min(i+serviceapi.EnrichmentBatchLimit, len(ids))]
			entities, err := s.api.ListEntities(ctx, installation, entityType, batch)
			if err != nil {
				// Budget pressure: stop decorating and retry on a later poll.
				return out
			}
			found := map[int64]string{}
			for _, entity := range entities {
				if !entity.Invalid {
					found[entity.ID] = entity.Name
				}
			}
			s.mu.Lock()
			if len(s.entityNames)+len(batch) > entityNameCacheLimit {
				for key, v := range s.entityNames {
					if now.After(v.until) {
						delete(s.entityNames, key)
					}
				}
			}
			for _, id := range batch {
				out[entityType+":"+strconv.FormatInt(id, 10)] = found[id]
				if len(s.entityNames) < entityNameCacheLimit {
					s.entityNames[entityKey{installation, entityType, id}] = cachedName{name: found[id], until: now.Add(entityNameTTL)}
				}
			}
			s.mu.Unlock()
		}
	}
	return out
}

// relatedLabels maps cards linked to the open lead through its contacts and
// companies to that participant's name. Callers apply the labels only to the
// employee's own tasks, so no other card is disclosed.
func (s *Service) relatedLabels(ctx context.Context, api employeeTaskAPI, installation uuid.UUID, lead int64) map[string]string {
	key := relatedKey{installation, lead}
	s.mu.Lock()
	entry, ok := s.related[key]
	s.mu.Unlock()
	if ok && time.Now().Before(entry.until) {
		return entry.labels
	}
	contacts, companies, err := api.LeadParticipants(ctx, installation, lead)
	if err != nil {
		return nil
	}
	labels := map[string]string{}
	refs := 0
	for _, group := range []struct {
		entityType string
		ids        []int64
	}{{"contacts", contacts}, {"companies", companies}} {
		for _, id := range group.ids {
			if refs++; refs > relatedParticipants {
				break
			}
			name, leads, err := api.ParticipantLeads(ctx, installation, group.entityType, id)
			if err != nil {
				return nil
			}
			if name == "" {
				name = entityLabels[group.entityType] + " #" + strconv.FormatInt(id, 10)
			}
			labels[group.entityType+":"+strconv.FormatInt(id, 10)] = name
			for _, linked := range leads {
				if linked != lead {
					labels["leads:"+strconv.FormatInt(linked, 10)] = name
				}
			}
		}
	}
	s.mu.Lock()
	if len(s.related) >= relatedCacheLimit {
		now := time.Now()
		for k, v := range s.related {
			if now.After(v.until) {
				delete(s.related, k)
			}
		}
	}
	if len(s.related) < relatedCacheLimit {
		s.related[key] = cachedRelated{labels: labels, until: time.Now().Add(relatedTTL)}
	}
	s.mu.Unlock()
	return labels
}

var _ serviceapi.EmployeeTasks = (*Service)(nil)
