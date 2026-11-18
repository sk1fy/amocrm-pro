package companycrm

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type Provider interface {
	VerifyCompanyCRMAccount(context.Context, uuid.UUID, int64) error
	CompanyCRMStaff(context.Context, uuid.UUID) ([]amocrm.CompanyCRMUser, error)
	CompanyCRMCompanies(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMCompany, error)
	CompanyCRMContacts(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMContact, error)
	CompanyCRMFields(context.Context, uuid.UUID, string) ([]amocrm.CompanyCRMField, error)
	CompanyCRMPipelines(context.Context, uuid.UUID, int64) ([]amocrm.CompanyCRMPipeline, error)
}

type snapshot[T any] struct {
	response         Response[T]
	attemptedBinding Binding
	retryAt          time.Time
}

type Service struct {
	Resolver Resolver
	CRM      Provider
	init     sync.Once
	gate     chan struct{}
	staff    snapshot[[]StaffMember]
	clients  snapshot[Clients]
	workflow snapshot[WorkflowMetadata]
	now      func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

func read[T any](ctx context.Context, s *Service, cache *snapshot[T], fetch func(context.Context, Binding) (T, error)) Response[T] {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.init.Do(func() { s.gate = make(chan struct{}, 1) })
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return Response[T]{Source: Source{State: "error", ErrorCode: "deadline_exceeded", RetryAfter: 1}}
	}
	b, err := s.Resolver.Resolve(ctx)
	now := s.clock()
	if err != nil {
		return failed(cache, now, "binding_unavailable", time.Minute)
	}
	if cache.attemptedBinding == b && now.Before(cache.retryAt) {
		return cache.response
	}
	cache.attemptedBinding = b
	started := now
	err = s.CRM.VerifyCompanyCRMAccount(ctx, b.InstallationID, b.AccountID)
	var data T
	if err == nil {
		data, err = fetch(ctx, b)
	}
	if err == nil {
		var current Binding
		current, err = s.Resolver.Resolve(ctx)
		if err == nil && current != b {
			err = ErrBinding
		}
	}
	if err == nil {
		fresh := started.Add(5 * time.Minute)
		*cache = snapshot[T]{attemptedBinding: b, retryAt: started.Add(time.Minute), response: Response[T]{Source: Source{AccountID: b.AccountID, BindingRevision: b.Revision, State: "fresh", Complete: true, ObservedAt: &started, FreshUntil: &fresh}, Data: &data}}
		return cache.response
	}

	retry := time.Minute
	code := "source_unavailable"
	var api *amocrm.APIError
	if errors.As(err, &api) && api.RetryAfter > 0 {
		retry = api.RetryAfter
		code = string(api.Kind)
	}
	if errors.Is(err, ErrBinding) {
		code = "binding_changed"
	}
	result := failed(cache, now, code, retry)
	if result.Data == nil {
		result.Source.AccountID = b.AccountID
		result.Source.BindingRevision = b.Revision
		cache.response = result
	}
	return result
}

func failed[T any](cache *snapshot[T], now time.Time, code string, retry time.Duration) Response[T] {
	cache.response.Source.State = "error"
	if cache.response.Data != nil {
		cache.response.Source.State = "stale"
	}
	cache.response.Source.ErrorCode = code
	cache.response.Source.RetryAfter = int64((retry + time.Second - 1) / time.Second)
	cache.retryAt = now.Add(retry)
	return cache.response
}

func (s *Service) Staff(ctx context.Context) Response[[]StaffMember] {
	return read(ctx, s, &s.staff, func(ctx context.Context, b Binding) ([]StaffMember, error) {
		users, err := s.CRM.CompanyCRMStaff(ctx, b.InstallationID)
		if err != nil {
			return nil, err
		}
		result := make([]StaffMember, 0, len(users))
		for _, u := range users {
			activity := "unknown"
			if u.Rights.Active != nil {
				activity = "inactive"
				if *u.Rights.Active {
					activity = "active"
				}
			}
			result = append(result, StaffMember{ID: u.ID, Name: u.Name, Email: u.Email, Activity: activity})
		}
		return result, nil
	})
}
func (s *Service) Clients(ctx context.Context) Response[Clients] {
	return read(ctx, s, &s.clients, func(ctx context.Context, b Binding) (Clients, error) {
		result := Clients{Companies: []Client{}, Contacts: []Contact{}}
		mappingVerified := false
		if b.AccountFieldID > 0 {
			fields, err := s.CRM.CompanyCRMFields(ctx, b.InstallationID, "companies")
			if err != nil {
				return result, err
			}
			count := 0
			for _, f := range fields {
				if f.ID == b.AccountFieldID {
					count++
					mappingVerified = f.Type == "numeric"
				}
			}
			if count != 1 || !mappingVerified {
				return result, ErrBinding
			}
		}
		companies, err := s.CRM.CompanyCRMCompanies(ctx, b.InstallationID, b.AccountID)
		if err != nil {
			return result, err
		}
		contacts, err := s.CRM.CompanyCRMContacts(ctx, b.InstallationID, b.AccountID)
		if err != nil {
			return result, err
		}
		accounts := map[int64]int{}
		for _, company := range companies {
			row := Client{CompanyID: company.ID, Name: company.Name, AccountIDs: []int64{}, Resolution: "unknown"}
			if mappingVerified {
				ids, valid := company.CompanyCRMAccountIDs(b.AccountFieldID)
				if valid {
					row.AccountIDs = ids
					row.Resolution = "zero"
					if len(ids) > 0 {
						row.Resolution = "one"
					}
				}
			}
			for _, id := range row.AccountIDs {
				accounts[id]++
			}
			result.Companies = append(result.Companies, row)
		}
		for i, row := range result.Companies {
			for _, id := range row.AccountIDs {
				if accounts[id] > 1 {
					result.Companies[i].Resolution = "multiple"
				}
			}
		}
		for _, contact := range contacts {
			row := Contact{ID: contact.ID, Name: contact.Name, CompanyIDs: []int64{}}
			for _, company := range contact.Embedded.Companies {
				row.CompanyIDs = append(row.CompanyIDs, company.ID)
			}
			result.Contacts = append(result.Contacts, row)
		}
		return result, nil
	})
}
func (s *Service) WorkflowMetadata(ctx context.Context) Response[WorkflowMetadata] {
	return read(ctx, s, &s.workflow, func(ctx context.Context, b Binding) (WorkflowMetadata, error) {
		result := WorkflowMetadata{RequiredFieldsState: "unknown"}
		var err error
		result.Pipelines, err = s.CRM.CompanyCRMPipelines(ctx, b.InstallationID, b.AccountID)
		if err != nil {
			return result, err
		}
		result.LeadFields, err = s.CRM.CompanyCRMFields(ctx, b.InstallationID, "leads")
		return result, err
	})
}
func (s *Service) ValidateWorkflow(ctx context.Context, r WorkflowRequest) WorkflowValidation {
	metadata := s.WorkflowMetadata(ctx)
	result := WorkflowValidation{Source: metadata.Source, Code: "source_unavailable", Scope: "pipeline_stage"}
	if metadata.Source.State != "fresh" || metadata.Data == nil {
		return result
	}
	result.Code = "binding_mismatch"
	if r.BindingRevision != metadata.Source.BindingRevision {
		return result
	}
	result.Code = "invalid_stage"
	for _, pipeline := range metadata.Data.Pipelines {
		if pipeline.ID == r.PipelineID {
			for _, stage := range pipeline.Embedded.Statuses {
				if stage.ID == r.StageID {
					result.Valid = true
					result.Code = "pipeline_stage_valid"
					return result
				}
			}
		}
	}
	return result
}
