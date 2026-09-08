package servicerpc

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
)

type gatewayServer struct {
	pb.UnimplementedGatewayServer
	impl serviceapi.Gateway
}
type gatewayClient struct{ remote pb.GatewayClient }

func (s *gatewayServer) Events(ctx context.Context, r *pb.EventPageRequest) (*pb.EventPage, error) {
	v, err := s.impl.Events(ctx, fromEventPageRequest(r))
	if err != nil {
		return nil, err
	}
	return toEventPage(v), nil
}
func (c gatewayClient) Events(ctx context.Context, r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	v, err := c.remote.Events(ctx, toEventPageRequest(r))
	if err != nil {
		return serviceapi.EventPage{}, err
	}
	return fromEventPage(v), nil
}
func (s *gatewayServer) Users(ctx context.Context, r *pb.UsersRequest) (*pb.Directory, error) {
	v, err := s.impl.Users(ctx, fromUsersRequest(r))
	if err != nil {
		return nil, err
	}
	return toDirectory(v), nil
}
func (c gatewayClient) Users(ctx context.Context, r serviceapi.UsersRequest) (serviceapi.Directory, error) {
	v, err := c.remote.Users(ctx, toUsersRequest(r))
	if err != nil {
		return serviceapi.Directory{}, err
	}
	return fromDirectory(v), nil
}
func (s *gatewayServer) Notes(ctx context.Context, r *pb.NotesRequest) (*pb.NotePage, error) {
	v, err := s.impl.Notes(ctx, fromNotesRequest(r))
	if err != nil {
		return nil, err
	}
	return toNotePage(v), nil
}
func (c gatewayClient) Notes(ctx context.Context, r serviceapi.NotesRequest) (serviceapi.NotePage, error) {
	v, err := c.remote.Notes(ctx, toNotesRequest(r))
	if err != nil {
		return serviceapi.NotePage{}, err
	}
	return fromNotePage(v), nil
}
func (s *gatewayServer) Tasks(ctx context.Context, r *pb.TasksRequest) (*pb.TaskPage, error) {
	v, err := s.impl.Tasks(ctx, fromTasksRequest(r))
	if err != nil {
		return nil, err
	}
	return toTaskPage(v), nil
}
func (c gatewayClient) Tasks(ctx context.Context, r serviceapi.TasksRequest) (serviceapi.TaskPage, error) {
	v, err := c.remote.Tasks(ctx, toTasksRequest(r))
	if err != nil {
		return serviceapi.TaskPage{}, err
	}
	return fromTaskPage(v), nil
}
func (s *gatewayServer) Pipelines(ctx context.Context, r *pb.CatalogRequest) (*pb.PipelineCatalog, error) {
	v, err := s.impl.Pipelines(ctx, fromCatalogRequest(r))
	if err != nil {
		return nil, err
	}
	return toPipelineCatalog(v), nil
}
func (c gatewayClient) Pipelines(ctx context.Context, r serviceapi.CatalogRequest) (serviceapi.PipelineCatalog, error) {
	v, err := c.remote.Pipelines(ctx, toCatalogRequest(r))
	if err != nil {
		return serviceapi.PipelineCatalog{}, err
	}
	return fromPipelineCatalog(v), nil
}
func (s *gatewayServer) CustomFields(ctx context.Context, r *pb.CustomFieldsRequest) (*pb.CustomFieldCatalog, error) {
	v, err := s.impl.CustomFields(ctx, fromCustomFieldsRequest(r))
	if err != nil {
		return nil, err
	}
	return toCustomFieldCatalog(v), nil
}
func (c gatewayClient) CustomFields(ctx context.Context, r serviceapi.CustomFieldsRequest) (serviceapi.CustomFieldCatalog, error) {
	v, err := c.remote.CustomFields(ctx, toCustomFieldsRequest(r))
	if err != nil {
		return serviceapi.CustomFieldCatalog{}, err
	}
	return fromCustomFieldCatalog(v), nil
}
func (s *gatewayServer) Entities(ctx context.Context, r *pb.EntitiesRequest) (*pb.EntityCatalog, error) {
	v, err := s.impl.Entities(ctx, fromEntitiesRequest(r))
	if err != nil {
		return nil, err
	}
	return toEntityCatalog(v), nil
}
func (c gatewayClient) Entities(ctx context.Context, r serviceapi.EntitiesRequest) (serviceapi.EntityCatalog, error) {
	v, err := c.remote.Entities(ctx, toEntitiesRequest(r))
	if err != nil {
		return serviceapi.EntityCatalog{}, err
	}
	return fromEntityCatalog(v), nil
}

type eventsServer struct {
	pb.UnimplementedCRMEventsServer
	impl serviceapi.CRMEvents
}
type eventsClient struct{ remote pb.CRMEventsClient }

func (s *eventsServer) GetEvent(ctx context.Context, r *pb.EventRequest) (*pb.Event, error) {
	impl, ok := s.impl.(serviceapi.EventReader)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "event detail reader unavailable")
	}
	v, err := impl.GetEvent(ctx, fromEventRequest(r))
	if err != nil {
		return nil, err
	}
	return toEvent(v), nil
}
func (c eventsClient) GetEvent(ctx context.Context, r serviceapi.EventRequest) (serviceapi.Event, error) {
	v, err := c.remote.GetEvent(ctx, toEventRequest(r))
	if err != nil {
		return serviceapi.Event{}, err
	}
	return fromEvent(v), nil
}
func (s *eventsServer) Apply(ctx context.Context, r *pb.Command) (*pb.Operation, error) {
	v, err := s.impl.Apply(ctx, fromCommand(r))
	if err != nil {
		return nil, err
	}
	return toOperation(v), nil
}
func (c eventsClient) Apply(ctx context.Context, r serviceapi.Command) (serviceapi.Operation, error) {
	v, err := c.remote.Apply(ctx, toCommand(r))
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return fromOperation(v), nil
}
func (s *eventsServer) QueryEvents(ctx context.Context, r *pb.Query) (*pb.QueryResult, error) {
	v, err := s.impl.Query(ctx, fromQuery(r))
	if err != nil {
		return nil, err
	}
	return toQueryResult(v), nil
}
func (c eventsClient) Query(ctx context.Context, r serviceapi.Query) (serviceapi.QueryResult, error) {
	v, err := c.remote.QueryEvents(ctx, toQuery(r))
	if err != nil {
		return serviceapi.QueryResult{}, err
	}
	result := fromQueryResult(v)
	return result, serviceapi.RequireQueryVersion(r, result)
}
func (s *eventsServer) Status(ctx context.Context, r *pb.Auth) (*pb.SyncStatus, error) {
	v, err := s.impl.Status(ctx, fromAuth(r))
	if err != nil {
		return nil, err
	}
	return toSyncStatus(v), nil
}
func (c eventsClient) Status(ctx context.Context, r serviceapi.Auth) (serviceapi.SyncStatus, error) {
	v, err := c.remote.Status(ctx, toAuth(r))
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	return fromSyncStatus(v), nil
}
func (s *eventsServer) OperationStatus(ctx context.Context, r *pb.OperationRequest) (*pb.Operation, error) {
	v, err := s.impl.Operation(ctx, fromOperationRequest(r))
	if err != nil {
		return nil, err
	}
	return toOperation(v), nil
}
func (c eventsClient) Operation(ctx context.Context, r serviceapi.OperationRequest) (serviceapi.Operation, error) {
	v, err := c.remote.OperationStatus(ctx, toOperationRequest(r))
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return fromOperation(v), nil
}

type activityServer struct {
	pb.UnimplementedActivityServer
	impl serviceapi.Activity
}
type activityClient struct{ remote pb.ActivityClient }

func (s *activityServer) GetPanel(ctx context.Context, r *pb.Query) (*pb.Panel, error) {
	v, err := s.impl.Panel(ctx, fromQuery(r))
	if err != nil {
		return nil, err
	}
	return toPanel(v), nil
}
func (c activityClient) Panel(ctx context.Context, r serviceapi.Query) (serviceapi.Panel, error) {
	v, err := c.remote.GetPanel(ctx, toQuery(r))
	if err != nil {
		return serviceapi.Panel{}, err
	}
	result := fromPanel(v)
	return result, serviceapi.RequireQueryVersion(r, result.Data)
}
func (s *activityServer) GetEvent(ctx context.Context, r *pb.EventRequest) (*pb.Event, error) {
	impl, ok := s.impl.(serviceapi.EventPresenter)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "event presenter unavailable")
	}
	v, err := impl.EventCard(ctx, fromEventRequest(r))
	if err != nil {
		return nil, err
	}
	return toEvent(v), nil
}
func (c activityClient) EventCard(ctx context.Context, r serviceapi.EventRequest) (serviceapi.Event, error) {
	v, err := c.remote.GetEvent(ctx, toEventRequest(r))
	if err != nil {
		return serviceapi.Event{}, err
	}
	return fromEvent(v), nil
}
func (s *activityServer) GetSettings(ctx context.Context, r *pb.Auth) (*pb.Settings, error) {
	v, err := s.impl.Settings(ctx, fromAuth(r))
	if err != nil {
		return nil, err
	}
	return toSettings(v), nil
}
func (c activityClient) Settings(ctx context.Context, r serviceapi.Auth) (serviceapi.Settings, error) {
	v, err := c.remote.GetSettings(ctx, toAuth(r))
	if err != nil {
		return serviceapi.Settings{}, err
	}
	return fromSettings(v), nil
}
func (s *activityServer) Configure(ctx context.Context, r *pb.SettingsCommand) (*pb.Operation, error) {
	v, err := s.impl.Configure(ctx, fromSettingsCommand(r))
	if err != nil {
		return nil, err
	}
	return toOperation(v), nil
}
func (c activityClient) Configure(ctx context.Context, r serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	v, err := c.remote.Configure(ctx, toSettingsCommand(r))
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return fromOperation(v), nil
}
func (s *activityServer) OperationStatus(ctx context.Context, r *pb.OperationRequest) (*pb.Operation, error) {
	v, err := s.impl.Operation(ctx, fromOperationRequest(r))
	if err != nil {
		return nil, err
	}
	return toOperation(v), nil
}
func (c activityClient) Operation(ctx context.Context, r serviceapi.OperationRequest) (serviceapi.Operation, error) {
	v, err := c.remote.OperationStatus(ctx, toOperationRequest(r))
	if err != nil {
		return serviceapi.Operation{}, err
	}
	return fromOperation(v), nil
}

type policyServer struct {
	pb.UnimplementedPolicyServer
	impl serviceapi.Policy
}
type policyClient struct{ remote pb.PolicyClient }

func (s *policyServer) Issue(ctx context.Context, r *pb.IssueRequest) (*pb.Auth, error) {
	v, err := s.impl.Issue(ctx, fromIssueRequest(r))
	if err != nil {
		return nil, err
	}
	return toAuth(v), nil
}
func (s *policyServer) Validate(ctx context.Context, r *pb.ValidateRequest) (*pb.Principal, error) {
	v, err := s.impl.Validate(ctx, fromAuth(r.GetAuth()), r.GetAudience(), r.GetAction())
	if err != nil {
		return nil, err
	}
	return toPrincipal(v), nil
}
func (c policyClient) Issue(ctx context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	v, err := c.remote.Issue(ctx, toIssueRequest(r))
	if err != nil {
		return serviceapi.Auth{}, err
	}
	return fromAuth(v), nil
}
func (c policyClient) Validate(ctx context.Context, r serviceapi.Auth, audience, action string) (serviceapi.Principal, error) {
	v, err := c.remote.Validate(ctx, &pb.ValidateRequest{Auth: toAuth(r), Audience: audience, Action: action})
	if err != nil {
		return serviceapi.Principal{}, err
	}
	return fromPrincipal(v), nil
}
