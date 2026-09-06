package servicerpc

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
)

type bootstrapServer struct {
	pb.UnimplementedCoreBootstrapServer
	impl serviceapi.CoreBootstrap
}

func (s *bootstrapServer) GetAccount(ctx context.Context, r *pb.BootstrapAccountRequest) (*pb.BootstrapAccount, error) {
	result, err := s.impl.GetAccount(ctx, fromBootstrapAccountRequest(r))
	if err != nil {
		return nil, err
	}
	return toBootstrapAccount(result), nil
}

type bootstrapClient struct{ remote pb.CoreBootstrapClient }

func (c bootstrapClient) GetAccount(ctx context.Context, r serviceapi.BootstrapAccountRequest) (serviceapi.BootstrapAccount, error) {
	result, err := c.remote.GetAccount(ctx, toBootstrapAccountRequest(r))
	if err != nil {
		return serviceapi.BootstrapAccount{}, err
	}
	return fromBootstrapAccount(result), nil
}
