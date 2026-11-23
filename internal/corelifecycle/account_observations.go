package corelifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

type AccountObservationService struct {
	store *Store
	crm   *amocrm.Client
}

func NewAccountObservationService(store *Store, crm *amocrm.Client) *AccountObservationService {
	return &AccountObservationService{store: store, crm: crm}
}

func (s *AccountObservationService) ObserveAccount(ctx context.Context, scope serviceapi.AccountObservationScope, component serviceapi.AccountObservationComponent) (serviceapi.AccountObservation, error) {
	if scope.Account <= 0 || scope.Installation == uuid.Nil || scope.Integration == uuid.Nil ||
		(scope.ObservationOnly && (scope.Generation != 0 || scope.AuthorizationSequence != 0)) || (!scope.ObservationOnly && scope.Generation <= 0) || scope.Generation > wire.MaxPosition ||
		scope.AuthorizationSequence < scope.Generation || scope.AuthorizationSequence > wire.MaxPosition ||
		(component != serviceapi.AccountObservationIdentity && component != serviceapi.AccountObservationUsers) {
		return serviceapi.AccountObservation{}, serviceapi.Fail(serviceapi.InvalidArgument, "invalid account observation scope or component")
	}
	before, err := s.checkAuthority(ctx, scope)
	if err != nil {
		return serviceapi.AccountObservation{}, err
	}
	var result serviceapi.AccountObservationResult
	var providerErr error
	if component == serviceapi.AccountObservationIdentity {
		var identity serviceapi.ObservedAccountIdentity
		identity, providerErr = s.crm.ObserveAccountIdentity(ctx, scope.Installation, scope.Account, before.domain)
		result = identity
	} else {
		var users serviceapi.ObservedCRMUserRoster
		users, providerErr = s.crm.ObserveCRMUsers(ctx, scope.Installation)
		result = users
	}
	observedAt := time.Now().UTC()
	if ctx.Err() != nil {
		return serviceapi.AccountObservation{}, ctx.Err()
	}
	after, err := s.checkAuthority(ctx, scope)
	if err != nil {
		return serviceapi.AccountObservation{}, err
	}
	if before != after {
		return serviceapi.AccountObservation{}, serviceapi.Fail(serviceapi.PermissionDenied, "account observation scope changed")
	}
	if providerErr != nil {
		return serviceapi.AccountObservation{}, observationProviderError(providerErr)
	}
	return serviceapi.AccountObservation{ObservedAt: observedAt, Result: result}, nil
}

func (s *AccountObservationService) checkAuthority(ctx context.Context, scope serviceapi.AccountObservationScope) (platformInstallationAuthority, error) {
	if scope.ObservationOnly {
		return s.legacyObservationAuthority(ctx, scope)
	}
	authority, err := s.store.platformInstallationAuthority(ctx, scope.Account, scope.Installation, scope.Integration)
	switch {
	case errors.Is(err, ErrAccessProofDenied), errors.Is(err, ErrAccessProofPolicy):
		return platformInstallationAuthority{}, serviceapi.Fail(serviceapi.PermissionDenied, "account observation scope denied")
	case err != nil:
		if ctx.Err() != nil {
			return platformInstallationAuthority{}, ctx.Err()
		}
		return platformInstallationAuthority{}, fmt.Errorf("%w: %w", serviceapi.Fail(serviceapi.Unavailable, "Core account observation scope unavailable"), err)
	case authority.generation != scope.Generation || authority.authorizationSequence != scope.AuthorizationSequence:
		return platformInstallationAuthority{}, serviceapi.Fail(serviceapi.PermissionDenied, "account observation scope changed")
	default:
		return authority, nil
	}
}

func observationProviderError(cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", serviceapi.Fail(serviceapi.DeadlineExceeded, "amoCRM observation timed out"), cause)
	}
	failure := &serviceapi.Error{Code: serviceapi.Unavailable, Message: "amoCRM observation unavailable"}
	var upstream *amocrm.APIError
	if errors.As(cause, &upstream) {
		switch upstream.Kind {
		case amocrm.ErrorRateLimited, amocrm.ErrorOverloaded:
			failure = &serviceapi.Error{Code: serviceapi.ResourceExhausted, Message: "amoCRM observation rate limited", RetryAfter: upstream.RetryAfter}
		case amocrm.ErrorUnauthorized:
			failure = &serviceapi.Error{Code: serviceapi.ReauthRequired, Message: "amoCRM authorization required for observation"}
		}
	}
	return fmt.Errorf("%w: %w", failure, cause)
}
