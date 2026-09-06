package crmevents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestCommandBoundsAndStableHash(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 7, Consumer: "activity"}
	a, err := normalizeCommand(serviceapi.Command{CommandID: "1", Kind: "sync", Auth: serviceapi.Auth{Token: "expired"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Auth.Token = "renewed"
	if string(commandHash(a, p)) != string(commandHash(b, p)) {
		t.Fatal("transport reissue changed command identity")
	}
	b.InitialDays++
	if string(commandHash(a, p)) == string(commandHash(b, p)) {
		t.Fatal("different payload has same identity")
	}
	for _, c := range []serviceapi.Command{{Kind: "sync"}, {CommandID: "x", Kind: "unexpected"}, {CommandID: "x", Kind: "sync", InitialDays: 8}, {CommandID: "x", Kind: "sync", InitialDays: 7, RetentionDays: 2}, {CommandID: "x", Kind: "backfill", From: 10, To: 9}} {
		if _, err := normalizeCommand(c, now); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
func TestDigestDetectsChangedPageAndCanonicalizesJSON(t *testing.T) {
	a := serviceapi.Event{ID: "same", CreatedAt: 1, ValueAfter: json.RawMessage(`{"b":2,"a":1}`)}
	b := a
	b.ValueAfter = json.RawMessage(`{ "a":1, "b":2 }`)
	x, _ := pageDigest("", []serviceapi.Event{a})
	y, _ := pageDigest("", []serviceapi.Event{b})
	if x != y {
		t.Fatal("JSON formatting counted as mutation")
	}
	b.ID = "shifted"
	y, _ = pageDigest("", []serviceapi.Event{b})
	if x == y {
		t.Fatal("shifted page remained stable")
	}
}
func TestUnauthorizedLocalCallsDoNotReachStorage(t *testing.T) {
	service := New(nil, deniedPolicy{}, nil, DefaultConfig())
	if _, err := service.Query(context.Background(), serviceapi.Query{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	if _, err := service.Apply(context.Background(), serviceapi.Command{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	if _, err := service.Status(context.Background(), serviceapi.Auth{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
	if _, err := service.Operation(context.Background(), serviceapi.OperationRequest{}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal(err)
	}
}

type deniedPolicy struct{}

func (deniedPolicy) Issue(context.Context, serviceapi.IssueRequest) (serviceapi.Auth, error) {
	return serviceapi.Auth{}, errors.New("denied")
}
func (deniedPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "denied")
}
