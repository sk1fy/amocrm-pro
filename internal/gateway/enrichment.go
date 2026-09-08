package gateway

import (
	"context"
	"encoding/json"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"time"
)

const catalogTTL = 5 * time.Minute

func catalogEntityType(kind string) (string, error) {
	entityType, ok := serviceapi.CatalogEntityType(kind)
	if !ok {
		return "", serviceapi.Fail(serviceapi.InvalidArgument, "unsupported entity type")
	}
	return entityType, nil
}

func validateCatalog(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return serviceapi.Fail(serviceapi.Internal, "invalid catalog")
	}
	if len(encoded) > maxCachedDirectoryBytes {
		return serviceapi.Fail(serviceapi.ResourceExhausted, "catalog exceeds v0 cache size limit")
	}
	return nil
}

func (s *Service) Notes(ctx context.Context, r serviceapi.NotesRequest) (serviceapi.NotePage, error) {
	entityType, err := catalogEntityType(r.EntityType)
	if err != nil {
		return serviceapi.NotePage{}, err
	}
	if err := serviceapi.ValidateIDBatch(r.IDs, serviceapi.EnrichmentBatchLimit); err != nil {
		return serviceapi.NotePage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionNotes)
	if err != nil {
		return serviceapi.NotePage{}, err
	}
	notes, err := s.api.ListNotes(ctx, p.InstallationID, entityType, r.IDs)
	if err != nil {
		return serviceapi.NotePage{}, corepolicy.MapUpstreamError(err)
	}
	result := serviceapi.NotePage{Notes: make([]serviceapi.Note, 0, len(notes))}
	for _, n := range notes {
		if n.Invalid {
			result.InvalidIDs = append(result.InvalidIDs, n.ID)
			continue
		}
		result.Notes = append(result.Notes, serviceapi.Note{ID: n.ID, EntityID: n.EntityID, EntityType: entityType, NoteType: n.NoteType, CreatedBy: n.CreatedBy, UpdatedAt: n.UpdatedAt, Params: n.Params})
	}
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.NotePage{}, err
	}
	return result, nil
}

func (s *Service) Tasks(ctx context.Context, r serviceapi.TasksRequest) (serviceapi.TaskPage, error) {
	if err := serviceapi.ValidateIDBatch(r.IDs, serviceapi.EnrichmentBatchLimit); err != nil {
		return serviceapi.TaskPage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionTasks)
	if err != nil {
		return serviceapi.TaskPage{}, err
	}
	tasks, err := s.api.ListTasks(ctx, p.InstallationID, r.IDs)
	if err != nil {
		return serviceapi.TaskPage{}, corepolicy.MapUpstreamError(err)
	}
	result := serviceapi.TaskPage{Tasks: make([]serviceapi.Task, 0, len(tasks))}
	for _, task := range tasks {
		if task.Invalid {
			result.InvalidIDs = append(result.InvalidIDs, task.ID)
			continue
		}
		result.Tasks = append(result.Tasks, serviceapi.Task{ID: task.ID, EntityID: task.EntityID, EntityType: task.EntityType, ResponsibleUserID: task.ResponsibleUserID, Text: task.Text, CompleteTill: task.CompleteTill, TaskTypeID: task.TaskTypeID, IsCompleted: task.IsCompleted, ResultText: task.ResultText, UpdatedAt: task.UpdatedAt})
	}
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.TaskPage{}, err
	}
	return result, nil
}

func (s *Service) Pipelines(ctx context.Context, r serviceapi.CatalogRequest) (serviceapi.PipelineCatalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionPipelines)
	if err != nil {
		return serviceapi.PipelineCatalog{}, err
	}
	s.mu.Lock()
	entry, ok := s.pipelines[p.InstallationID]
	s.mu.Unlock()
	if !ok || time.Now().After(entry.until) {
		data, err := s.api.ListPipelines(ctx, p.InstallationID)
		if err != nil {
			return serviceapi.PipelineCatalog{}, corepolicy.MapUpstreamError(err)
		}
		if err := validateCatalog(data); err != nil {
			return serviceapi.PipelineCatalog{}, err
		}
		entry = cachedPipelines{data: data, fetchedAt: time.Now().Unix(), until: time.Now().Add(catalogTTL)}
		s.mu.Lock()
		if len(s.pipelines) >= 256 {
			now := time.Now()
			for id, v := range s.pipelines {
				if now.After(v.until) {
					delete(s.pipelines, id)
				}
			}
		}
		if len(s.pipelines) < 256 {
			s.pipelines[p.InstallationID] = entry
		}
		s.mu.Unlock()
	}
	result := mapPipelines(entry)
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.PipelineCatalog{}, err
	}
	return result, nil
}

func (s *Service) CustomFields(ctx context.Context, r serviceapi.CustomFieldsRequest) (serviceapi.CustomFieldCatalog, error) {
	entityType, err := catalogEntityType(r.EntityType)
	if err != nil {
		return serviceapi.CustomFieldCatalog{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionCustomFields)
	if err != nil {
		return serviceapi.CustomFieldCatalog{}, err
	}
	key := fieldCacheKey{installation: p.InstallationID, entityType: entityType}
	s.mu.Lock()
	entry, ok := s.fields[key]
	s.mu.Unlock()
	if !ok || time.Now().After(entry.until) {
		data, err := s.api.ListCustomFields(ctx, p.InstallationID, entityType)
		if err != nil {
			return serviceapi.CustomFieldCatalog{}, corepolicy.MapUpstreamError(err)
		}
		if err := validateCatalog(data); err != nil {
			return serviceapi.CustomFieldCatalog{}, err
		}
		entry = cachedFields{data: data, fetchedAt: time.Now().Unix(), until: time.Now().Add(catalogTTL)}
		s.mu.Lock()
		if len(s.fields) >= 256 {
			now := time.Now()
			for id, v := range s.fields {
				if now.After(v.until) {
					delete(s.fields, id)
				}
			}
		}
		if len(s.fields) < 256 {
			s.fields[key] = entry
		}
		s.mu.Unlock()
	}
	result := mapCustomFields(entityType, entry)
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.CustomFieldCatalog{}, err
	}
	return result, nil
}

func (s *Service) Entities(ctx context.Context, r serviceapi.EntitiesRequest) (serviceapi.EntityCatalog, error) {
	entityType, err := catalogEntityType(r.EntityType)
	if err != nil {
		return serviceapi.EntityCatalog{}, err
	}
	if err := serviceapi.ValidateIDBatch(r.IDs, serviceapi.EnrichmentBatchLimit); err != nil {
		return serviceapi.EntityCatalog{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.policy.Validate(ctx, r.Auth, serviceapi.GatewayService, serviceapi.ActionEntities)
	if err != nil {
		return serviceapi.EntityCatalog{}, err
	}
	entities, err := s.api.ListEntities(ctx, p.InstallationID, entityType, r.IDs)
	if err != nil {
		return serviceapi.EntityCatalog{}, corepolicy.MapUpstreamError(err)
	}
	result := serviceapi.EntityCatalog{Entities: make([]serviceapi.EntityName, 0, len(entities))}
	for _, entity := range entities {
		if entity.Invalid {
			result.InvalidIDs = append(result.InvalidIDs, entity.ID)
			continue
		}
		result.Entities = append(result.Entities, serviceapi.EntityName{ID: entity.ID, EntityType: entityType, Name: entity.Name})
	}
	if err := serviceapi.ValidateResponseSize(result); err != nil {
		return serviceapi.EntityCatalog{}, err
	}
	return result, nil
}

func mapPipelines(entry cachedPipelines) serviceapi.PipelineCatalog {
	result := serviceapi.PipelineCatalog{FetchedAt: entry.fetchedAt, Pipelines: make([]serviceapi.Pipeline, 0, len(entry.data))}
	for _, pipe := range entry.data {
		mapped := serviceapi.Pipeline{ID: pipe.ID, Name: pipe.Name, Statuses: make([]serviceapi.PipelineStatus, 0, len(pipe.Statuses))}
		for _, status := range pipe.Statuses {
			mapped.Statuses = append(mapped.Statuses, serviceapi.PipelineStatus{ID: status.ID, Name: status.Name})
		}
		result.Pipelines = append(result.Pipelines, mapped)
	}
	return result
}

func mapCustomFields(entityType string, entry cachedFields) serviceapi.CustomFieldCatalog {
	result := serviceapi.CustomFieldCatalog{FetchedAt: entry.fetchedAt, Fields: make([]serviceapi.CustomField, 0, len(entry.data))}
	for _, field := range entry.data {
		mapped := serviceapi.CustomField{ID: field.ID, Name: field.Name, Type: field.Type, EntityType: entityType, Enums: make([]serviceapi.CustomFieldEnum, 0, len(field.Enums))}
		for _, enum := range field.Enums {
			mapped.Enums = append(mapped.Enums, serviceapi.CustomFieldEnum{ID: enum.ID, Value: enum.Value})
		}
		result.Fields = append(result.Fields, mapped)
	}
	return result
}

var _ serviceapi.Gateway = (*Service)(nil)
