package serviceapi

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type AccountObservationComponent uint8

const (
	AccountObservationIdentity AccountObservationComponent = iota + 1
	AccountObservationUsers
)

type AccountObservationScope struct {
	ObservationOnly           bool
	Installation, Integration uuid.UUID
	Account                   int64
	Generation                int64
	AuthorizationSequence     int64
}

type ObservedAccountIdentity struct {
	WidgetVersions map[string]string
	CurrentDomain  string
	DisplayName    string
	TimeZone       *string
}

func (ObservedAccountIdentity) isAccountObservation() {}

type ObservedAccountPaymentRequired struct{}

func (ObservedAccountPaymentRequired) isAccountObservation() {}

type ObservedCRMUser struct {
	ID       int64
	Name     string
	IsActive bool
	IsAdmin  bool
}

type ObservedCRMUserRoster []ObservedCRMUser

func (ObservedCRMUserRoster) isAccountObservation() {}

type AccountObservationResult interface{ isAccountObservation() }

type AccountObservation struct {
	ObservedAt time.Time
	Result     AccountObservationResult
}

type CoreAccountObservations interface {
	ObserveAccount(context.Context, AccountObservationScope, AccountObservationComponent) (AccountObservation, error)
}
