package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
)

func TestActivityPeriodLocalAndMTLSParity(t *testing.T) {
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
	client := dialTest(t, ca, address, serviceapi.CoreService).Activity.(serviceapi.ActivityPeriodReader)
	auth, err := corepolicy.ForCaller(policy, serviceapi.CoreService).Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindViewer, PanelID: id, ViewKeyVersion: 1, Consumer: serviceapi.ActivityService, RequestID: "period-parity", Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionView)})
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Europe/Moscow")
	now := time.Now().In(loc)
	cutoff := now.Unix()
	req := serviceapi.ActivityPeriodRequest{Auth: auth, Kind: "week", Anchor: now.Format(time.DateOnly), StartMinutes: 540, EndMinutes: 1080, CutoffUnix: &cutoff, EmployeeIDs: []int64{7}}
	local, err := owner.ViewActivityPeriod(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.ViewActivityPeriod(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	local.SnapshotAt, remote.SnapshotAt = 0, 0
	if !reflect.DeepEqual(local, remote) || len(remote.Employees) != 1 || remote.CutoffUnix != cutoff || remote.Previous.Cut == nil {
		t.Fatalf("period mismatch local%+v remote%+v", local, remote)
	}
	daysReq := serviceapi.ActivityPeriodDaysRequest{Auth: auth, EmployeeID: 7, FromDay: remote.Current.Start, ToDay: remote.Current.End, StartMinutes: 540, EndMinutes: 1080, CutoffUnix: &cutoff}
	localDays, err := owner.ViewActivityPeriodDays(ctx, daysReq)
	if err != nil {
		t.Fatal(err)
	}
	remoteDays, err := client.ViewActivityPeriodDays(ctx, daysReq)
	if err != nil {
		t.Fatal(err)
	}
	localDays.SnapshotAt, remoteDays.SnapshotAt = 0, 0
	if !reflect.DeepEqual(localDays, remoteDays) || len(remoteDays.Days) != 7 {
		t.Fatalf("period days mismatch local%+v remote%+v", localDays, remoteDays)
	}
	req.EmployeeIDs = []int64{99}
	if _, err = client.ViewActivityPeriod(ctx, req); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign employee=%v", err)
	}
	daysReq.EmployeeID = 99
	if _, err = client.ViewActivityPeriodDays(ctx, daysReq); serviceapi.ErrorCode(err) != serviceapi.NotFound {
		t.Fatalf("foreign employee days=%v", err)
	}
	foreign := dialTest(t, ca, address, serviceapi.EventsService).Activity.(serviceapi.ActivityPeriodReader)
	if _, err = foreign.ViewActivityPeriod(ctx, req); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatalf("non-Core caller=%v", err)
	}
}

type periodDeadlineOwner struct {
	serviceapi.Activity
	period, days time.Duration
}

func (o *periodDeadlineOwner) ViewActivityPeriod(ctx context.Context, _ serviceapi.ActivityPeriodRequest) (serviceapi.ActivityPeriod, error) {
	deadline, _ := ctx.Deadline()
	o.period = time.Until(deadline)
	return serviceapi.ActivityPeriod{}, nil
}
func (o *periodDeadlineOwner) ViewActivityPeriodDays(ctx context.Context, _ serviceapi.ActivityPeriodDaysRequest) (serviceapi.ActivityPeriodDays, error) {
	deadline, _ := ctx.Deadline()
	o.days = time.Until(deadline)
	return serviceapi.ActivityPeriodDays{}, nil
}

func TestActivityPeriodSpecificDeadlineOverMTLS(t *testing.T) {
	if rpcCallTimeout(pb.Activity_ViewActivityPeriod_FullMethodName) != 25*time.Second || rpcCallTimeout(pb.Activity_ViewActivityPeriodDays_FullMethodName) != 25*time.Second || rpcCallTimeout(pb.Activity_ViewPanel_FullMethodName) != 10*time.Second {
		t.Fatal("period timeout must not widen other RPC methods")
	}
	owner := &periodDeadlineOwner{}
	ca := newCA(t)
	address := start(t, ca, &Endpoints{Activity: owner})
	client := dialTest(t, ca, address, serviceapi.CoreService).Activity.(serviceapi.ActivityPeriodReader)
	if _, err := client.ViewActivityPeriod(context.Background(), serviceapi.ActivityPeriodRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ViewActivityPeriodDays(context.Background(), serviceapi.ActivityPeriodDaysRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []time.Duration{owner.period, owner.days} {
		if budget < 24*time.Second || budget > 25*time.Second {
			t.Fatalf("period RPC context budget=%v", budget)
		}
	}
	// An owner without the additive port fails explicitly.
	plain := start(t, ca, &Endpoints{Activity: struct{ serviceapi.Activity }{}})
	if _, err := dialTest(t, ca, plain, serviceapi.CoreService).Activity.(serviceapi.ActivityPeriodReader).ViewActivityPeriod(context.Background(), serviceapi.ActivityPeriodRequest{}); serviceapi.ErrorCode(err) != serviceapi.Unavailable {
		t.Fatalf("missing port=%v", err)
	}
}
