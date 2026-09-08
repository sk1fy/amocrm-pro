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

func TestGatewayEnrichmentMTLSAllowsOnlyCRMEvents(t *testing.T) {
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
			func() error { _, err := client.Pipelines(ctx, pipelines); return err },
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
	}
	for _, method := range []string{pb.Gateway_Notes_FullMethodName + "Extra", pb.Gateway_Tasks_FullMethodName + "Extra", pb.Gateway_Pipelines_FullMethodName + "Extra", pb.Gateway_CustomFields_FullMethodName + "Extra", pb.Gateway_Entities_FullMethodName + "Extra"} {
		if allowedCaller(method, serviceapi.EventsService) {
			t.Fatalf("suffix Extra allowed: %s", method)
		}
	}
}
