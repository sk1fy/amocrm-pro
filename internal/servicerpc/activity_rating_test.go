package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
)

func TestActivityRatingLocalAndMTLSParity(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	repo := &parityRepository{panels: map[uuid.UUID]serviceapi.ManagedPanel{id: {ID: id, Enabled: true, ViewKeyVersion: 1, EmployeeIDs: []int64{7}, Timezone: "Europe/Moscow"}}}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, serviceapi.GatewayService))
	events := &parityEvents{policy: corepolicy.ForCaller(policy, serviceapi.EventsService)}
	dependencyAddress := start(t, ca, &Endpoints{Gateway: gw, CRMEvents: events})
	dependencies := dialTest(t, ca, dependencyAddress, serviceapi.ActivityService)
	owner := product.New(repo, corepolicy.ForCaller(policy, serviceapi.ActivityService), dependencies.CRMEvents, dependencies.Gateway)
	address := start(t, ca, &Endpoints{Activity: owner})
	client := dialTest(t, ca, address, serviceapi.CoreService).Activity.(serviceapi.ActivityRatingReader)
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: id, ViewKeyVersion: 1, Consumer: serviceapi.ActivityService, RequestID: "rating-parity", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := int64(200)
	req := serviceapi.ActivityRatingRequest{Auth: auth, From: 90, To: 200, Day: "1970-01-01", Surface: "profile", EmployeeIDs: []int64{7}, StartMinutes: 0, EndMinutes: 1440, CutoffUnix: &cutoff}
	local, err := owner.ViewActivityRatings(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.ViewActivityRatings(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	local.SnapshotAt = 0
	remote.SnapshotAt = 0
	if !reflect.DeepEqual(local, remote) || len(remote.Employees) != 1 || len(remote.Employees[0].MinutePercentages) != 1441 || remote.CutoffUnix == nil || *remote.CutoffUnix != cutoff {
		t.Fatalf("rating mismatch local%+v remote%+v", local, remote)
	}
	req.EmployeeIDs = []int64{99}
	if _, err = client.ViewActivityRatings(ctx, req); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign employee=%v", err)
	}
	foreign := dialTest(t, ca, address, serviceapi.EventsService).Activity.(serviceapi.ActivityRatingReader)
	if _, err = foreign.ViewActivityRatings(ctx, req); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("non-Core caller=%v", err)
	}
}
