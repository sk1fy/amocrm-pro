package servicerpc

import (
	"context"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type platformAccountObservationsServer struct {
	platformpb.UnimplementedCoreAccountObservationsServer
	impl serviceapi.CoreAccountObservations
}

func (s *platformAccountObservationsServer) ObserveAccount(ctx context.Context, req *platformpb.ObserveAccountRequest) (*platformpb.AccountObservation, error) {
	installation, installationErr := uuid.Parse(req.GetCoreInstallationId())
	integration, integrationErr := uuid.Parse(req.GetIntegrationId())
	if installationErr != nil || integrationErr != nil {
		return nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid account observation scope")
	}
	var component serviceapi.AccountObservationComponent
	switch req.GetComponent() {
	case platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_LICENSE:
		component = serviceapi.AccountObservationIdentity
	case platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS:
		component = serviceapi.AccountObservationUsers
	default:
		return nil, serviceapi.Fail(serviceapi.InvalidArgument, "invalid account observation component")
	}
	result, err := s.impl.ObserveAccount(ctx, serviceapi.AccountObservationScope{
		ObservationOnly: req.GetObservationOnly(), Installation: installation, Integration: integration, Account: req.GetAmoAccountId(),
		Generation: int64(req.GetGeneration()), AuthorizationSequence: int64(req.GetAuthorizationSequence()),
	}, component)
	if err != nil {
		return nil, err
	}
	response := &platformpb.AccountObservation{ObservedAt: timestamppb.New(result.ObservedAt)}
	switch observed := result.Result.(type) {
	case serviceapi.ObservedAccountIdentity:
		response.Result = &platformpb.AccountObservation_Identity{Identity: &platformpb.AccountIdentity{
			CurrentDomain: observed.CurrentDomain, DisplayName: observed.DisplayName, TimeZone: observed.TimeZone, WidgetVersions: observed.WidgetVersions,
		}}
	case serviceapi.ObservedAccountPaymentRequired:
		response.Result = &platformpb.AccountObservation_PaymentRequired{PaymentRequired: &platformpb.AccountPaymentRequired{}}
	case serviceapi.ObservedCRMUserRoster:
		roster := &platformpb.CRMUserRoster{Users: make([]*platformpb.ObservedCRMUser, 0, len(observed))}
		for _, user := range observed {
			roster.Users = append(roster.Users, &platformpb.ObservedCRMUser{
				Id: user.ID, Name: user.Name, IsActive: user.IsActive, IsAdmin: user.IsAdmin,
			})
		}
		response.Result = &platformpb.AccountObservation_Roster{Roster: roster}
	default:
		return nil, serviceapi.Fail(serviceapi.Internal, "invalid account observation result")
	}
	return response, nil
}
