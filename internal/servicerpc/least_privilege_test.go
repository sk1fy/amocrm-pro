package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"sync/atomic"
	"testing"
)

type aclEvents struct {
	serviceapi.CRMEvents
	applies atomic.Int32
}

func (s *aclEvents) Apply(context.Context, serviceapi.Command) (serviceapi.Operation, error) {
	s.applies.Add(1)
	return serviceapi.Operation{State: serviceapi.OperationAccepted}, nil
}
func (*aclEvents) Query(context.Context, serviceapi.Query) (serviceapi.QueryResult, error) {
	return serviceapi.QueryResult{}, nil
}
func (*aclEvents) Status(context.Context, serviceapi.Auth) (serviceapi.SyncStatus, error) {
	return serviceapi.SyncStatus{}, nil
}
func (*aclEvents) Operation(context.Context, serviceapi.OperationRequest) (serviceapi.Operation, error) {
	return serviceapi.Operation{State: serviceapi.OperationSucceeded}, nil
}

func TestCRMEventsMTLSActivityCannotApply(t *testing.T) {
	ca := newCA(t)
	receiver := &aclEvents{}
	address := start(t, ca, &Endpoints{CRMEvents: receiver})
	activity := dialTest(t, ca, address, serviceapi.ActivityService)
	core := dialTest(t, ca, address, serviceapi.CoreService)
	ctx := context.Background()
	if _, err := activity.CRMEvents.Apply(ctx, serviceapi.Command{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("Activity Apply: %v", err)
	}
	if receiver.applies.Load() != 0 {
		t.Fatal("denied caller reached receiver")
	}
	if _, err := activity.CRMEvents.Query(ctx, serviceapi.Query{}); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.CRMEvents.Status(ctx, serviceapi.Auth{}); err != nil {
		t.Fatal(err)
	}
	if _, err := activity.CRMEvents.Operation(ctx, serviceapi.OperationRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := core.CRMEvents.Apply(ctx, serviceapi.Command{}); err != nil {
		t.Fatal(err)
	}
	if receiver.applies.Load() != 1 {
		t.Fatal("Core Apply did not reach receiver")
	}
	for _, method := range []string{pb.CRMEvents_Apply_FullMethodName, "/amocrm.services.v1.CRMEvents/FutureMutation", pb.CRMEvents_QueryEvents_FullMethodName + "Extra"} {
		if allowedCaller(method, serviceapi.ActivityService) {
			t.Fatalf("unrecognized/mutating method allowed: %s", method)
		}
	}
}

func TestGatewayMTLSRestrictsEnrichmentAndPipelineCallers(t *testing.T) {
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	address := start(t, ca, &Endpoints{Policy: policy, Gateway: gw})
	auth, err := dialTest(t, ca, address, serviceapi.EventsService).Policy.Issue(context.Background(), serviceapi.IssueRequest{Scope: scope, System: true, Consumer: serviceapi.ActivityService, RequestID: "enrich-acl", Grants: []serviceapi.Grant{{Audience: serviceapi.GatewayService, Action: serviceapi.ActionNotes}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionTasks}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionPipelines}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionCustomFields}, {Audience: serviceapi.GatewayService, Action: serviceapi.ActionEntities}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	notes := serviceapi.NotesRequest{Auth: auth, EntityType: "leads", IDs: []int64{1}}
	tasks := serviceapi.TasksRequest{Auth: auth, IDs: []int64{1}}
	pipelines := serviceapi.CatalogRequest{Auth: auth}
	fields := serviceapi.CustomFieldsRequest{Auth: auth, EntityType: "leads"}
	entities := serviceapi.EntitiesRequest{Auth: auth, EntityType: "leads", IDs: []int64{1}}
	for _, identity := range []string{serviceapi.CoreService, serviceapi.ActivityService, serviceapi.GatewayService, serviceapi.EventsService} {
		client := dialTest(t, ca, address, identity).Gateway
		for _, call := range []func() error{
			func() error { _, err := client.Notes(ctx, notes); return err },
			func() error { _, err := client.Tasks(ctx, tasks); return err },
			func() error { _, err := client.CustomFields(ctx, fields); return err },
			func() error { _, err := client.Entities(ctx, entities); return err },
		} {
			err := call()
			if identity == serviceapi.EventsService {
				if err != nil {
					t.Fatalf("crm-events denied: %v", err)
				}
			} else if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
				t.Fatalf("%s reached enrichment: %v", identity, err)
			}
		}
		_, pipelineErr := client.Pipelines(ctx, pipelines)
		if identity == serviceapi.EventsService || identity == serviceapi.ActivityService {
			if pipelineErr != nil {
				t.Fatalf("%s pipelines denied: %v", identity, pipelineErr)
			}
		} else if serviceapi.ErrorCode(pipelineErr) != serviceapi.PermissionDenied {
			t.Fatalf("%s reached pipelines: %v", identity, pipelineErr)
		}
	}
	for _, method := range []string{pb.Gateway_Notes_FullMethodName + "Extra", pb.Gateway_Tasks_FullMethodName + "Extra", pb.Gateway_Pipelines_FullMethodName + "Extra", pb.Gateway_CustomFields_FullMethodName + "Extra", pb.Gateway_Entities_FullMethodName + "Extra"} {
		if allowedCaller(method, serviceapi.EventsService) {
			t.Fatalf("suffix Extra allowed: %s", method)
		}
	}
}

func TestGatewayProblemsMTLSAllowsActivityWithViewerGrant(t *testing.T) {
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	address := start(t, ca, &Endpoints{Gateway: gw})
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(context.Background(), serviceapi.IssueRequest{
		Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: uuid.New(), ViewKeyVersion: 1,
		Consumer: serviceapi.ActivityService, RequestID: "viewer-problems", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{serviceapi.CoreService, serviceapi.ActivityService, serviceapi.GatewayService, serviceapi.EventsService} {
		client := dialTest(t, ca, address, identity).Gateway
		_, err := client.(serviceapi.ProblemReader).Problems(context.Background(), serviceapi.ProblemRequest{Auth: auth, EmployeeIDs: []int64{7}})
		want := serviceapi.PermissionDenied
		if identity == serviceapi.ActivityService {
			// No snapshot store is configured: reaching that error proves both
			// mTLS admission and viewer-grant validation succeeded.
			want = serviceapi.Unavailable
		}
		if serviceapi.ErrorCode(err) != want {
			t.Fatalf("%s problems: got %v, want %s", identity, err, want)
		}
	}
	client := dialTest(t, ca, address, serviceapi.ActivityService).Gateway
	_, err = client.(serviceapi.ProblemReader).Problems(context.Background(), serviceapi.ProblemRequest{Auth: serviceapi.Auth{Token: "forged"}, EmployeeIDs: []int64{7}})
	if serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
		t.Fatalf("forged viewer accepted: %v", err)
	}
	if allowedCaller(pb.Gateway_Problems_FullMethodName+"Extra", serviceapi.ActivityService) {
		t.Fatal("unknown problems method admitted")
	}
}

func TestEmployeeTaskRPCsAreActivityOnly(t *testing.T) {
	for _, method := range []string{pb.Gateway_OwnTasks_FullMethodName, pb.Gateway_OwnTask_FullMethodName, pb.Gateway_CompleteOwnTask_FullMethodName, pb.Gateway_OwnLead_FullMethodName} {
		if !allowedCaller(method, serviceapi.ActivityService) {
			t.Fatalf("Activity cannot relay %s", method)
		}
		for _, caller := range []string{serviceapi.EventsService, serviceapi.CoreService, serviceapi.GatewayService, "unknown"} {
			if allowedCaller(method, caller) {
				t.Fatalf("%s may call %s", caller, method)
			}
		}
	}
	for _, method := range []string{pb.Activity_OwnTasks_FullMethodName, pb.Activity_OwnTask_FullMethodName, pb.Activity_CompleteOwnTask_FullMethodName, pb.Activity_OwnLead_FullMethodName} {
		if !allowedCaller(method, serviceapi.CoreService) || allowedCaller(method, serviceapi.EventsService) || allowedCaller(method, serviceapi.ActivityService) {
			t.Fatalf("Activity task RPC %s must be Core-only", method)
		}
	}
}
