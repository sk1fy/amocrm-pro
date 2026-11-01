package servicerpc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/requesttiming"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type timingGateway struct {
	taskGateway
	expected uuid.UUID
	delay    bool
	disabled bool
}

func (g timingGateway) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	observed := requesttiming.RequestID(ctx)
	if !requesttiming.Enabled(ctx) {
		observed = httpmiddleware.RequestIDFromContext(ctx)
	}
	if g.disabled && requesttiming.Enabled(ctx) {
		return serviceapi.TaskQueue{}, serviceapi.Fail(serviceapi.Internal, "unexpected diagnostics")
	}
	if observed != g.expected {
		return serviceapi.TaskQueue{}, serviceapi.Fail(serviceapi.Internal, "correlation lost")
	}
	end := requesttiming.Measure(ctx, requesttiming.AmoHTTP)
	if g.delay {
		<-ctx.Done()
		end(ctx.Err())
		return serviceapi.TaskQueue{}, ctx.Err()
	}
	end(nil)
	return g.taskGateway.OwnTasks(ctx, r)
}

type timingActivity struct {
	serviceapi.Activity
	serviceapi.EmployeeTasks
}

func startTimingServer(t *testing.T, ca certAuthority, identity string, endpoints *Endpoints) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(ca.config(t, identity, true), endpoints)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return l.Addr().String()
}
func TestTimingTwoHopMTLSMergesOnceAndKeepsCorrelation(t *testing.T) {
	ca := newCA(t)
	id := uuid.New()
	gatewayAddress := startTimingServer(t, ca, "gateway", &Endpoints{Gateway: timingGateway{expected: id}})
	gateway := dialTest(t, ca, gatewayAddress, "activity").Gateway.(serviceapi.EmployeeTasks)
	activityAddress := startTimingServer(t, ca, "activity", &Endpoints{Activity: timingActivity{EmployeeTasks: gateway}})
	activity := dialTest(t, ca, activityAddress, "core").Activity.(serviceapi.EmployeeTasks)
	ctx := requesttiming.Start(context.Background(), id, requesttiming.Core, requesttiming.Local)
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs(timingID, "secret", timingEnabled, "garbage", timingSummary, "secret"))
	if _, err := activity.OwnTasks(ctx, serviceapi.OwnTasksRequest{}); err != nil {
		t.Fatal(err)
	}
	s := requesttiming.Snapshot(ctx)
	if s.Incomplete {
		t.Fatalf("valid chain incomplete: %+v", s)
	}
	counts := map[requesttiming.Stage]uint32{}
	for i := 0; i < s.Count; i++ {
		c := s.Cells[i]
		counts[c.Stage] += c.Count
		if c.Method != requesttiming.OwnTasks {
			t.Fatalf("method attribution lost: %+v", c)
		}
	}
	if counts[requesttiming.RPCClient] != 2 || counts[requesttiming.RPCServer] != 2 || counts[requesttiming.AmoHTTP] != 1 {
		t.Fatalf("lost or double-merged child: %v", counts)
	}
	if strings.Contains(s.ServerTiming(), "secret") {
		t.Fatal("metadata leaked")
	}
}
func TestTimingDeadlineBeforeTrailerIsIncomplete(t *testing.T) {
	ca := newCA(t)
	id := uuid.New()
	address := startTimingServer(t, ca, "gateway", &Endpoints{Gateway: timingGateway{expected: id, delay: true}})
	client := dialTest(t, ca, address, "activity").Gateway.(serviceapi.EmployeeTasks)
	ctx := requesttiming.Start(context.Background(), id, requesttiming.Core, requesttiming.Local)
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := client.OwnTasks(ctx, serviceapi.OwnTasksRequest{}); serviceapi.ErrorCode(err) != serviceapi.DeadlineExceeded {
		t.Fatalf("deadline changed: %v", err)
	}
	s := requesttiming.Snapshot(ctx)
	if !s.Incomplete {
		t.Fatal("missing remote evidence reported complete")
	}
	for i := 0; i < s.Count; i++ {
		if s.Cells[i].Stage == requesttiming.AmoHTTP {
			t.Fatal("invented remote HTTP duration")
		}
	}
}
func TestTimingMetadataValidatedAfterMTLSAuthorization(t *testing.T) {
	ca := newCA(t)
	address := startTimingServer(t, ca, "gateway", &Endpoints{Gateway: taskGateway{}})
	rawClient := func(identity string) pb.GatewayClient {
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(ca.config(t, identity, false))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pb.NewGatewayClient(conn)
	}
	raw := rawClient("crm-events")
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(timingID, uuid.NewString(), timingEnabled, "1"))
	var trailers metadata.MD
	if _, err := raw.OwnTasks(ctx, &pb.OwnTasksRequest{}, grpc.Trailer(&trailers)); status.Code(err) != codes.PermissionDenied || len(trailers.Get(timingSummary)) != 0 {
		t.Fatalf("timing bypassed auth: %v %v", err, trailers)
	}
	client := rawClient("activity")
	valid := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(timingID, uuid.NewString(), timingEnabled, "1"))
	if _, err := client.OwnTasks(valid, &pb.OwnTasksRequest{}, grpc.Trailer(&trailers)); err != nil || len(trailers.Get(timingSummary)) != 1 {
		t.Fatalf("valid timing control failed: %v %v", err, trailers)
	}
	for _, md := range []metadata.MD{metadata.Pairs(timingID, uuid.NewString(), timingID, uuid.NewString(), timingEnabled, "1"), metadata.Pairs(timingID, uuid.NewString(), timingEnabled, "1", timingEnabled, "1"), metadata.Pairs(timingID, uuid.Nil.String(), timingEnabled, "1"), metadata.Pairs(timingID, "secret", timingEnabled, "1")} {
		trailers = nil
		if _, err := client.OwnTasks(metadata.NewOutgoingContext(context.Background(), md), &pb.OwnTasksRequest{}, grpc.Trailer(&trailers)); err != nil {
			t.Fatal(err)
		}
		if len(trailers.Get(timingSummary)) != 0 {
			t.Fatal("accepted malformed or duplicate timing metadata")
		}
	}
}

func TestTimingRejectsMissingMalformedDuplicateOrOversizeTrailers(t *testing.T) {
	for name, values := range map[string][]string{"missing": nil, "malformed": {"customer-secret"}, "duplicate": {"{}", "{}"}, "oversize": {strings.Repeat("x", requesttiming.MaxWireBytes+1)}} {
		t.Run(name, func(t *testing.T) {
			ca := newCA(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(ca.config(t, "gateway", true))), grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				if len(values) > 0 {
					grpc.SetTrailer(ctx, metadata.MD{timingSummary: values})
				}
				return handler(ctx, req)
			}))
			pb.RegisterGatewayServer(server, &gatewayServer{impl: taskGateway{}})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			client := dialTest(t, ca, listener.Addr().String(), "activity").Gateway.(serviceapi.EmployeeTasks)
			ctx := requesttiming.Start(context.Background(), uuid.New(), requesttiming.Core, requesttiming.Local)
			if _, err := client.OwnTasks(ctx, serviceapi.OwnTasksRequest{}); err != nil {
				t.Fatal(err)
			}
			s := requesttiming.Snapshot(ctx)
			if !s.Incomplete || strings.Contains(s.ServerTiming(), "secret") {
				t.Fatalf("invalid trailer accepted: %+v", s)
			}
		})
	}
}

type timingFullPolicy struct{ serviceapi.Policy }

func (p timingFullPolicy) Issue(ctx context.Context, _ serviceapi.IssueRequest) (serviceapi.Auth, error) {
	for _, stage := range []requesttiming.Stage{requesttiming.EmployeeVerify, requesttiming.SQL, requesttiming.Acquire, requesttiming.TokenLookup, requesttiming.Limiter, requesttiming.AmoHTTP} {
		end := requesttiming.Measure(ctx, stage)
		end(nil)
	}
	requesttiming.ObserveLimits(ctx, requesttiming.Limits{PairRPS: 7, AccountRPS: 50, PairBurst: 1, AccountBurst: 1, MaxWaitMS: 5000, PairWaiters: 64, AccountWaiters: 512, ProcessWaiters: 4096, Replicas: 1})
	return serviceapi.Auth{Token: "delegated"}, nil
}
func (p timingFullPolicy) Validate(ctx context.Context, _ serviceapi.Auth, _, _ string) (serviceapi.Principal, error) {
	for _, stage := range []requesttiming.Stage{requesttiming.SQL, requesttiming.Acquire} {
		end := requesttiming.Measure(ctx, stage)
		end(nil)
	}
	return serviceapi.Principal{}, nil
}

type timingFullActivity struct {
	timingActivity
	policy serviceapi.Policy
}

func (a timingFullActivity) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	if _, err := a.policy.Validate(ctx, r.Auth, serviceapi.ActivityService, serviceapi.ActionSessions); err != nil {
		return serviceapi.TaskQueue{}, err
	}
	for _, stage := range []requesttiming.Stage{requesttiming.SQL, requesttiming.Acquire} {
		end := requesttiming.Measure(ctx, stage)
		end(nil)
	}
	return a.EmployeeTasks.OwnTasks(ctx, r)
}

type timingFullGateway struct{ timingGateway }

func (g timingFullGateway) OwnTasks(ctx context.Context, r serviceapi.OwnTasksRequest) (serviceapi.TaskQueue, error) {
	for _, stage := range []requesttiming.Stage{requesttiming.SQL, requesttiming.Acquire, requesttiming.QueueLoad, requesttiming.QueueRefresh, requesttiming.TokenLookup, requesttiming.Limiter} {
		end := requesttiming.Measure(ctx, stage)
		end(nil)
	}
	requesttiming.Observe(ctx, requesttiming.QueueInitial, requesttiming.MissingRow)
	requesttiming.Observe(ctx, requesttiming.QueueFinal, requesttiming.Refreshed)
	return g.timingGateway.OwnTasks(ctx, r)
}
func TestTimingRepresentativeColdReadHeaderRemainsComplete(t *testing.T) {
	ca := newCA(t)
	id := uuid.New()
	workerAddress := startTimingServer(t, ca, "gateway", &Endpoints{Policy: timingFullPolicy{}, Gateway: timingFullGateway{timingGateway{expected: id}}})
	worker := dialTest(t, ca, workerAddress, "activity")
	activityAddress := startTimingServer(t, ca, "activity", &Endpoints{Activity: timingFullActivity{timingActivity{EmployeeTasks: worker.Gateway.(serviceapi.EmployeeTasks)}, worker.Policy}})
	coreWorker := dialTest(t, ca, workerAddress, "core")
	activity := dialTest(t, ca, activityAddress, "core").Activity.(serviceapi.EmployeeTasks)
	ctx := requesttiming.Start(context.Background(), id, requesttiming.Core, requesttiming.Local)
	finish := requesttiming.Measure(ctx, requesttiming.Handler)
	auth, err := coreWorker.Policy.Issue(ctx, serviceapi.IssueRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = activity.OwnTasks(ctx, serviceapi.OwnTasksRequest{Auth: auth}); err != nil {
		t.Fatal(err)
	}
	finish(nil)
	summary := requesttiming.Snapshot(ctx)
	header := summary.ServerTiming()
	if summary.Incomplete || strings.Contains(header, "complete=false") || strings.Contains(header, "truncated=true") || !strings.Contains(header, "gateway_limits") || !strings.Contains(header, "queue_initial_missing_row") || len(header) > requesttiming.MaxHeaderBytes {
		t.Fatalf("cold-read evidence lost (%d bytes, %d cells): %s", len(header), summary.Count, header)
	}
	t.Logf("representative cold read header: %d bytes, %d cells, complete", len(header), summary.Count)
}

func TestTimingDisabledStillPropagatesCorrelationAcrossTwoHops(t *testing.T) {
	ca := newCA(t)
	id := uuid.New()
	address := startTimingServer(t, ca, "gateway", &Endpoints{Gateway: timingGateway{expected: id, disabled: true}})
	gateway := dialTest(t, ca, address, "activity").Gateway.(serviceapi.EmployeeTasks)
	activityAddress := startTimingServer(t, ca, "activity", &Endpoints{Activity: timingActivity{EmployeeTasks: gateway}})
	activity := dialTest(t, ca, activityAddress, "core").Activity.(serviceapi.EmployeeTasks)
	ctx := httpmiddleware.ContextWithRequestID(context.Background(), id)
	if _, err := activity.OwnTasks(ctx, serviceapi.OwnTasksRequest{}); err != nil {
		t.Fatal(err)
	}
}
