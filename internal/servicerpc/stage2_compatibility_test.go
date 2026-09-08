package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestStage2MixedVersionReadOptionsFailClosed(t *testing.T) {
	ca := newCA(t)
	ctx := context.Background()
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: "mixed-version", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	oldEvents := responseEvents{result: serviceapi.QueryResult{}}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	local := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), oldEvents, gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, serviceapi.GatewayService)))
	remote := dialTest(t, ca, start(t, ca, &Endpoints{CRMEvents: oldEvents, Activity: local}), serviceapi.CoreService)
	base := serviceapi.Query{Auth: auth, From: 90, To: 200, UserIDs: []int64{7}, Limit: 10}
	for _, mutate := range []func(*serviceapi.Query){
		func(q *serviceapi.Query) { q.Types = []string{"task_added"} },
		func(q *serviceapi.Query) { q.TypePrefix = "task_" },
		func(q *serviceapi.Query) { q.EntityType = "task" },
		func(q *serviceapi.Query) { q.EntityType, q.EntityIDs = "task", []int64{41} },
		func(q *serviceapi.Query) { q.Order = "desc" },
		func(q *serviceapi.Query) { q.Compact = true },
		func(q *serviceapi.Query) { q.Cursor = "versioned-cursor" },
		func(q *serviceapi.Query) { q.Categories = []string{serviceapi.CategoryTasks} },
		func(q *serviceapi.Query) { q.IncludeUnknownAuthors = true },
		func(q *serviceapi.Query) { q.Buckets = serviceapi.BucketHour; q.Timezone = "UTC" },
	} {
		query := base
		mutate(&query)
		for _, call := range []func() error{
			func() error { _, err := remote.CRMEvents.Query(ctx, query); return err },
			func() error { _, err := local.Panel(ctx, query); return err },
			func() error { _, err := remote.Activity.Panel(ctx, query); return err },
		} {
			if err := call(); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
				query.Auth = serviceapi.Auth{}
				t.Fatalf("unsupported options silently accepted %+v: %v", query, err)
			}
		}
	}
	group := base
	group.GroupID = 9
	if _, err := remote.CRMEvents.Query(ctx, group); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("old Events ignored group_id: %v", err)
	}
	if _, err := remote.CRMEvents.Query(ctx, base); err != nil {
		t.Fatalf("old Events legacy query rejected: %v", err)
	}
	if _, err := local.Panel(ctx, base); err != nil {
		t.Fatalf("old owner legacy panel rejected: %v", err)
	}
	if _, err := remote.Activity.Panel(ctx, base); err != nil {
		t.Fatalf("old owner remote legacy panel rejected: %v", err)
	}
}

type stage2DetailACL struct {
	serviceapi.CRMEvents
	calls atomic.Int32
}

func (s *stage2DetailACL) GetEvent(context.Context, serviceapi.EventRequest) (serviceapi.Event, error) {
	s.calls.Add(1)
	return serviceapi.Event{ID: "synthetic", ValueBefore: []byte(`[]`), ValueAfter: []byte(`[]`)}, nil
}

func TestStage2EventDetailMTLSAllowsOnlyReadCallers(t *testing.T) {
	ca := newCA(t)
	receiver := &stage2DetailACL{}
	address := start(t, ca, &Endpoints{CRMEvents: receiver})
	for _, identity := range []string{serviceapi.CoreService, serviceapi.ActivityService, serviceapi.GatewayService, serviceapi.EventsService} {
		client := dialTest(t, ca, address, identity).CRMEvents
		reader, ok := client.(serviceapi.EventReader)
		if !ok {
			t.Fatal("RPC client omitted detail port")
		}
		before := receiver.calls.Load()
		_, err := reader.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "synthetic"})
		if identity == serviceapi.CoreService || identity == serviceapi.ActivityService {
			if err != nil || receiver.calls.Load() != before+1 {
				t.Fatalf("read caller %s denied: %v", identity, err)
			}
		} else if serviceapi.ErrorCode(err) != serviceapi.PermissionDenied || receiver.calls.Load() != before {
			t.Fatalf("non-reader %s reached detail owner: %v", identity, err)
		}
	}
	if allowedCaller(pb.CRMEvents_GetEvent_FullMethodName+"Extra", serviceapi.ActivityService) {
		t.Fatal("detail ACL must match an exact method")
	}
}

func TestStage2OldServerWithoutDetailReturnsUnavailable(t *testing.T) {
	ca := newCA(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(ca.config(t, serviceapi.EventsService, true))))
	// Register the actual pre-detail service shape so gRPC returns Unimplemented,
	// as an old deployed process would, instead of a mock domain error.
	desc := pb.CRMEvents_ServiceDesc
	desc.Methods = nil
	for _, method := range pb.CRMEvents_ServiceDesc.Methods {
		if method.MethodName != "GetEvent" {
			desc.Methods = append(desc.Methods, method)
		}
	}
	server.RegisterService(&desc, &pb.UnimplementedCRMEventsServer{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client := dialTest(t, ca, listener.Addr().String(), serviceapi.CoreService).CRMEvents.(serviceapi.EventReader)
	if _, err := client.GetEvent(context.Background(), serviceapi.EventRequest{EventID: "synthetic"}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("old detail endpoint should be an explicit unavailable capability: %v", err)
	}
}
