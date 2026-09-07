// Package servicerpc provides mTLS adapters over the same serviceapi business
// implementations used locally. It never contains product SQL or credentials.
package servicerpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"os"
	"strings"
	"time"
)

const MaxMessageSize = 4 << 20
const MaxConcurrent = 32
const RPCTimeout = 10 * time.Second

type Endpoints struct {
	BootstrapAccount serviceapi.CoreBootstrap
	Ready            func(context.Context) error
	Metrics          prometheus.Registerer

	Policy    serviceapi.Policy
	Gateway   serviceapi.Gateway
	Activity  serviceapi.Activity
	CRMEvents serviceapi.CRMEvents
}

func NewServer(config *tls.Config, endpoints *Endpoints) *grpc.Server {
	slots := make(chan struct{}, MaxConcurrent)
	var calls *prometheus.CounterVec
	var latency *prometheus.HistogramVec
	if endpoints.Metrics != nil {
		calls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "service_rpc_requests_total", Help: "Internal RPC requests by finite method and outcome."}, []string{"method", "code"})
		latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "service_rpc_duration_seconds", Help: "Internal RPC duration including policy."}, []string{"method"})
		endpoints.Metrics.MustRegister(calls, latency)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(config)), grpc.MaxRecvMsgSize(MaxMessageSize), grpc.MaxSendMsgSize(MaxMessageSize), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (result any, err error) {
		started := time.Now()
		defer func() {
			if calls != nil {
				calls.WithLabelValues(info.FullMethod, status.Code(err).String()).Inc()
				latency.WithLabelValues(info.FullMethod).Observe(time.Since(started).Seconds())
			}
		}()
		identity := peerIdentity(ctx)
		if !allowedCaller(info.FullMethod, identity) {
			return nil, status.Error(codes.PermissionDenied, "mTLS service identity is not authorized")
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			return nil, status.Error(codes.ResourceExhausted, "service concurrency quota exceeded")
		}
		ctx, cancel := context.WithTimeout(corepolicy.WithCaller(ctx, identity), RPCTimeout)
		defer cancel()
		result, err = handler(ctx, req)
		return result, toStatus(err)
	}), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return status.Error(codes.Unimplemented, "streaming is outside the v0 contract")
	}))
	if endpoints.BootstrapAccount != nil {
		pb.RegisterCoreBootstrapServer(server, &bootstrapServer{impl: endpoints.BootstrapAccount})
	}
	if endpoints.Policy != nil {
		pb.RegisterPolicyServer(server, &policyServer{impl: endpoints.Policy})
	}
	if endpoints.Gateway != nil {
		pb.RegisterGatewayServer(server, &gatewayServer{impl: endpoints.Gateway})
	}
	if endpoints.Activity != nil {
		pb.RegisterActivityServer(server, &activityServer{impl: endpoints.Activity})
	}
	if endpoints.CRMEvents != nil {
		pb.RegisterCRMEventsServer(server, &eventsServer{impl: endpoints.CRMEvents})
	}
	grpc_health_v1.RegisterHealthServer(server, &readinessServer{ready: endpoints.Ready})
	return server
}
func peerIdentity(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates) == 0 {
		return ""
	}
	identity := ""
	for _, uri := range info.State.PeerCertificates[0].URIs {
		if uri.Scheme == "spiffe" && uri.Host == "amocrm-pro" && uri.RawQuery == "" && uri.Fragment == "" {
			candidate := strings.TrimPrefix(uri.Path, "/")
			if knownIdentity(candidate) {
				if identity != "" && identity != candidate {
					return ""
				}
				identity = candidate
			}
		}
	}
	return identity
}
func knownIdentity(s string) bool {
	return s == serviceapi.CoreService || s == serviceapi.ActivityService || s == serviceapi.EventsService || s == serviceapi.GatewayService
}
func allowedCaller(method, caller string) bool {
	if !knownIdentity(caller) {
		return false
	}
	switch {
	case strings.HasPrefix(method, "/grpc.health.v1.Health/"):
		return true
	case strings.HasPrefix(method, "/amocrm.services.v1.CoreBootstrap/"):
		return caller == serviceapi.CoreService
	case strings.HasPrefix(method, "/amocrm.services.v1.Policy/"):
		return true
	case strings.HasPrefix(method, "/amocrm.services.v1.Activity/"):
		return caller == serviceapi.CoreService
	case strings.HasPrefix(method, "/amocrm.services.v1.CRMEvents/"):
		if caller == serviceapi.CoreService {
			return true
		}
		return caller == serviceapi.ActivityService && (method == pb.CRMEvents_QueryEvents_FullMethodName || method == pb.CRMEvents_Status_FullMethodName || method == pb.CRMEvents_OperationStatus_FullMethodName)
	case strings.HasPrefix(method, "/amocrm.services.v1.Gateway/Events"):
		return caller == serviceapi.EventsService
	case strings.HasPrefix(method, "/amocrm.services.v1.Gateway/Users"):
		return caller == serviceapi.ActivityService
	}
	return false
}

func TLSFromFiles(caFile, certFile, keyFile, serverName string, server bool) (*tls.Config, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid service CA")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots, ServerName: serverName}
	if server {
		config.ClientCAs = roots
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}

type Clients struct {
	BootstrapAccount serviceapi.CoreBootstrap
	Policy           serviceapi.Policy
	Gateway          serviceapi.Gateway
	Activity         serviceapi.Activity
	CRMEvents        serviceapi.CRMEvents
	conn             *grpc.ClientConn
}

// Connect constructs reusable clients without contacting optional dependencies.
// RPCs still use the same mTLS credentials, quotas and finite deadlines as Dial;
// connection failure is never a reason to switch to another implementation.
func Connect(address string, config *tls.Config) (*Clients, error) {
	conn, err := grpc.NewClient(address, clientOptions(config)...)
	if err != nil {
		return nil, err
	}
	return clientsFor(conn), nil
}

// Dial waits briefly for transport establishment. Standalone components and
// explicit connectivity checks use it when the dependency is startup-critical.
func Dial(ctx context.Context, address string, config *tls.Config) (*Clients, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	opts := append(clientOptions(config), grpc.WithBlock())
	conn, err := grpc.DialContext(ctx, address, opts...)
	if err != nil {
		return nil, err
	}
	return clientsFor(conn), nil
}
func clientOptions(config *tls.Config) []grpc.DialOption {
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(config)), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageSize), grpc.MaxCallSendMsgSize(MaxMessageSize)), grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, cancel := context.WithTimeout(ctx, RPCTimeout)
		defer cancel()
		return fromStatus(invoker(ctx, method, req, reply, cc, opts...))
	})}
}
func clientsFor(conn *grpc.ClientConn) *Clients {
	return &Clients{BootstrapAccount: bootstrapClient{pb.NewCoreBootstrapClient(conn)}, Policy: policyClient{pb.NewPolicyClient(conn)}, Gateway: gatewayClient{pb.NewGatewayClient(conn)}, Activity: activityClient{pb.NewActivityClient(conn)}, CRMEvents: eventsClient{pb.NewCRMEventsClient(conn)}, conn: conn}
}
func (c *Clients) Close() error { return c.conn.Close() }
func (c *Clients) Ready(ctx context.Context) error {
	r, err := grpc_health_v1.NewHealthClient(c.conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if r.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		return serviceapi.Fail(serviceapi.Unavailable, "service is not ready")
	}
	return nil
}
func (c *Clients) Connection() *grpc.ClientConn { return c.conn }

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	code := serviceapi.ErrorCode(err)
	mapping := map[serviceapi.Code]codes.Code{serviceapi.InvalidArgument: codes.InvalidArgument, serviceapi.Unauthenticated: codes.Unauthenticated, serviceapi.PermissionDenied: codes.PermissionDenied, serviceapi.NotFound: codes.NotFound, serviceapi.Conflict: codes.AlreadyExists, serviceapi.Unavailable: codes.Unavailable, serviceapi.DeadlineExceeded: codes.DeadlineExceeded, serviceapi.ResourceExhausted: codes.ResourceExhausted, serviceapi.ReauthRequired: codes.FailedPrecondition, serviceapi.Internal: codes.Internal}
	message := "internal service error"
	var domain *serviceapi.Error
	if errors.As(err, &domain) {
		message = domain.Message
	}
	s := status.New(mapping[code], message)
	s, _ = s.WithDetails(&errdetails.ErrorInfo{Reason: string(code), Domain: "amocrm-pro"})
	if domain != nil && domain.RetryAfter > 0 {
		s, _ = s.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(domain.RetryAfter)})
	}
	return s.Err()
}
func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	s, ok := status.FromError(err)
	if !ok {
		return serviceapi.Fail(serviceapi.Unavailable, "RPC unavailable")
	}
	code := serviceapi.Unavailable
	mapping := map[codes.Code]serviceapi.Code{codes.InvalidArgument: serviceapi.InvalidArgument, codes.Unauthenticated: serviceapi.Unauthenticated, codes.PermissionDenied: serviceapi.PermissionDenied, codes.NotFound: serviceapi.NotFound, codes.AlreadyExists: serviceapi.Conflict, codes.DeadlineExceeded: serviceapi.DeadlineExceeded, codes.ResourceExhausted: serviceapi.ResourceExhausted, codes.FailedPrecondition: serviceapi.ReauthRequired, codes.Internal: serviceapi.Internal}
	if v, ok := mapping[s.Code()]; ok {
		code = v
	}
	result := &serviceapi.Error{Code: code, Message: s.Message()}
	for _, detail := range s.Details() {
		if retry, ok := detail.(*errdetails.RetryInfo); ok {
			result.RetryAfter = retry.RetryDelay.AsDuration()
		}
	}
	return result
}

// Readiness observes dependencies on each check; Watch streaming is unsupported.
type readinessServer struct {
	grpc_health_v1.UnimplementedHealthServer
	ready func(context.Context) error
}

func (s *readinessServer) Check(ctx context.Context, r *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	if r.GetService() != "" {
		return nil, serviceapi.Fail(serviceapi.NotFound, "unknown health service")
	}
	state := grpc_health_v1.HealthCheckResponse_SERVING
	if s.ready != nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if s.ready(ctx) != nil {
			state = grpc_health_v1.HealthCheckResponse_NOT_SERVING
		}
	}
	return &grpc_health_v1.HealthCheckResponse{Status: state}, nil
}
