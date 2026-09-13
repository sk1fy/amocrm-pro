package activitybridge

import (
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestDeliveryIssueRequestUsesOperatorKindForActorZero(t *testing.T) {
	scope := serviceapi.Scope{}
	user := deliveryIssueRequest(scope, 7, "cmd", serviceapi.ActivityService, serviceapi.ActionSettings)
	if user.Kind != "" || user.ActorID != 7 {
		t.Fatalf("user issue=%+v", user)
	}
	operator := deliveryIssueRequest(scope, 0, "cmd", serviceapi.EventsService, serviceapi.ActionSync)
	if operator.Kind != serviceapi.PrincipalKindOperator || operator.ActorID != 0 || operator.Consumer != serviceapi.ActivityService {
		t.Fatalf("operator issue=%+v", operator)
	}
}

func TestPastRedeliveryHorizonUsesCreatedAtNotAttempts(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if pastRedeliveryHorizon(now.Add(-RedeliveryHorizon+time.Second), now) {
		t.Fatal("command still inside the calendar horizon")
	}
	if !pastRedeliveryHorizon(now.Add(-RedeliveryHorizon-time.Second), now) {
		t.Fatal("command older than the calendar horizon must expire")
	}
	if pastRedeliveryHorizon(now.Add(-RedeliveryHorizon), now) {
		t.Fatal("exact horizon boundary is still deliverable")
	}
}

func TestDeliveryExpiredErrorIsConflictAndTyped(t *testing.T) {
	err := ErrDeliveryExpired()
	if serviceapi.ErrorCode(err) != serviceapi.Conflict || !IsDeliveryExpired(err) {
		t.Fatalf("typed expired error=%v", err)
	}
	if IsDeliveryExpired(serviceapi.Fail(serviceapi.Conflict, "other")) || IsDeliveryExpired(serviceapi.Fail(serviceapi.NotFound, "failed delivery not found")) {
		t.Fatal("unrelated errors classified as delivery expired")
	}
}
