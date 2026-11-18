package servicerpc

import (
	"context"

	"github.com/google/uuid"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func platformPeer(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates) == 0 {
		return false
	}
	uris := info.State.PeerCertificates[0].URIs
	return len(uris) == 1 && uris[0].String() == wire.PlatformIdentity
}

type platformInstallationsServer struct {
	platformpb.UnimplementedCoreInstallationsServer
	impl serviceapi.CoreInstallations
}

func (s *platformInstallationsServer) GetSnapshot(ctx context.Context, req *platformpb.GetSnapshotRequest) (*platformpb.InstallationSnapshot, error) {
	id, err := uuid.Parse(req.GetCoreInstallationId())
	if err != nil || id == uuid.Nil {
		return nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid installation")
	}
	v, err := s.impl.GetSnapshot(ctx, req.GetAuth(), id)
	if err != nil {
		return nil, err
	}
	var authorization platformpb.Authorization
	switch v.Authorization {
	case wire.Authorized:
		authorization = platformpb.Authorization_AUTHORIZATION_AUTHORIZED
	case wire.ReauthRequired:
		authorization = platformpb.Authorization_AUTHORIZATION_REAUTH_REQUIRED
	case wire.Disabled:
		authorization = platformpb.Authorization_AUTHORIZATION_DISABLED
	default:
		authorization = platformpb.Authorization_AUTHORIZATION_UNKNOWN
	}
	return &platformpb.InstallationSnapshot{
		CoreInstallationId: v.InstallationID.String(), IntegrationId: v.IntegrationID.String(),
		AmoAccountId: v.AccountID, CurrentDomain: v.Domain, Authorization: authorization,
		ObservedAtUnix: v.ObservedAt.Unix(), Generation: v.Generation, Sequence: v.Sequence,
		AuthorizationSequence: v.AuthorizationSequence,
	}, nil
}
