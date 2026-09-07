package servicerpc

import (
	"context"
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
