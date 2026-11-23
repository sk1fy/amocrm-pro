package servicerpc

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type accountDirectoryServer struct {
	platformpb.UnimplementedCoreAccountDirectoryServer
	impl serviceapi.CoreAccountDirectory
}

func (s *accountDirectoryServer) ListInstallations(ctx context.Context, r *platformpb.AccountDirectoryRequest) (*platformpb.AccountDirectoryPage, error) {
	var after uuid.UUID
	var err error
	if r.GetAfterInstallationId() != "" {
		after, err = uuid.Parse(r.GetAfterInstallationId())
		if err != nil || after == uuid.Nil {
			return nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid directory cursor")
		}
	}
	items, next, err := s.impl.ListAccountInstallations(ctx, r.GetAmoAccountId(), after)
	if err != nil {
		return nil, err
	}
	result := &platformpb.AccountDirectoryPage{}
	for _, item := range items {
		result.Entries = append(result.Entries, &platformpb.AccountDirectoryEntry{InstallationId: item.Installation.String(), IntegrationId: item.Integration.String(), ProductCode: item.ProductCode, Authorization: item.Authorization, Current: item.Current, ObservationOnly: item.ObservationOnly, LifecycleSequence: item.LifecycleSequence, RegisteredAt: timestamppb.New(item.RegisteredAt)})
	}
	if next != uuid.Nil {
		result.NextCursor = next.String()
	}
	return result, nil
}
