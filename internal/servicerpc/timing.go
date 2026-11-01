package servicerpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/requesttiming"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const timingID = "x-amocrm-request-id"
const timingEnabled = "x-amocrm-timing"
const timingSummary = "x-amocrm-timing-summary"

func timingMethod(method string) requesttiming.Method {
	switch method {
	case pb.Policy_Issue_FullMethodName:
		return requesttiming.Issue
	case pb.Policy_Validate_FullMethodName:
		return requesttiming.Validate
	case pb.Activity_OwnTasks_FullMethodName, pb.Gateway_OwnTasks_FullMethodName:
		return requesttiming.OwnTasks
	case pb.Activity_OwnTask_FullMethodName, pb.Gateway_OwnTask_FullMethodName:
		return requesttiming.OwnTask
	case pb.Activity_OwnLead_FullMethodName, pb.Gateway_OwnLead_FullMethodName:
		return requesttiming.OwnLead
	case pb.Activity_CompleteOwnTask_FullMethodName, pb.Gateway_CompleteOwnTask_FullMethodName:
		return requesttiming.CompleteTask
	default:
		return requesttiming.Local
	}
}
func certificateService(config *tls.Config) (requesttiming.Service, bool) {
	if len(config.Certificates) == 0 || len(config.Certificates[0].Certificate) == 0 {
		return 0, false
	}
	cert, err := x509.ParseCertificate(config.Certificates[0].Certificate[0])
	if err != nil {
		return 0, false
	}
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" && u.Host == "amocrm-pro" && u.RawQuery == "" && u.Fragment == "" {
			if s, ok := requesttiming.ServiceForIdentity(strings.TrimPrefix(u.Path, "/")); ok {
				return s, true
			}
		}
	}
	return 0, false
}
func incomingTiming(ctx context.Context, service requesttiming.Service, method string) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	ids := md.Get(timingID)
	if len(ids) != 1 {
		return ctx
	}
	id, err := uuid.Parse(ids[0])
	if err != nil || id == uuid.Nil || id.String() != ids[0] {
		return ctx
	}
	ctx = httpmiddleware.ContextWithRequestID(ctx, id)
	on := md.Get(timingEnabled)
	if len(on) != 1 || on[0] != "1" {
		return ctx
	}
	return requesttiming.Start(ctx, id, service, timingMethod(method))
}
func invokeTiming(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Delete(timingID)
	md.Delete(timingEnabled)
	md.Delete(timingSummary)
	id := requesttiming.RequestID(ctx)
	if id == uuid.Nil {
		id = httpmiddleware.RequestIDFromContext(ctx)
	}
	md.Set(timingID, id.String())
	enabled := requesttiming.Enabled(ctx)
	if enabled {
		md.Set(timingEnabled, "1")
	}
	ctx = metadata.NewOutgoingContext(ctx, md)
	finish := requesttiming.MeasureMethod(ctx, requesttiming.RPCClient, timingMethod(method))
	var trailer metadata.MD
	var remote peer.Peer
	opts = append(opts, grpc.Trailer(&trailer), grpc.Peer(&remote))
	err := invoker(ctx, method, req, reply, cc, opts...)
	finish(timingError(err))
	if enabled {
		values := trailer.Get(timingSummary)
		identity := peerIdentity(peer.NewContext(ctx, &remote))
		service, ok := requesttiming.ServiceForIdentity(identity)
		if !ok || len(values) != 1 {
			requesttiming.Incomplete(ctx)
		} else if summary, decodeErr := requesttiming.Decode(values[0], service); decodeErr != nil {
			requesttiming.Incomplete(ctx)
		} else {
			requesttiming.Merge(ctx, summary)
		}
	}
	return err
}

func timingError(err error) error {
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.Canceled:
		return context.Canceled
	default:
		return err
	}
}
