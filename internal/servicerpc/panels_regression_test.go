package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"math"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"google.golang.org/protobuf/proto"
)

func TestPanelCommandProtobufRoundTripPreservesReplayIdentity(t *testing.T) {
	disabled := false
	want := serviceapi.PanelCommand{
		Auth: serviceapi.Auth{Token: "signed-token"}, CommandID: uuid.NewString(), PanelID: uuid.New(),
		Name: "Night", EmployeeIDs: []int64{1<<53 + 1, math.MaxInt64},
		DisplayWindow: serviceapi.DisplayWindow{From: "22:00", To: "06:00"}, Enabled: &disabled,
		Revision: math.MaxInt64, HasName: true, HasEmployees: true, HasWindow: true,
	}
	wire, err := proto.Marshal(toPanelCommand(want))
	if err != nil {
		t.Fatal(err)
	}
	var decoded pb.PanelCommand
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	got := fromPanelCommand(&decoded)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("protobuf changed patch command:\n got: %+v\nwant: %+v", got, want)
	}

	want.Enabled = nil
	wire, err = proto.Marshal(toPanelCommand(want))
	if err != nil {
		t.Fatal(err)
	}
	decoded.Reset()
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := fromPanelCommand(&decoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("protobuf changed nullable enabled or Has* flags: got %+v want %+v", got, want)
	}
}

func TestPanelPatchValidationLocalAndMTLS(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	events := &parityEvents{policy: corepolicy.ForCaller(policy, "crm-events")}
	local := product.New(repo, corepolicy.ForCaller(policy, "activity"), events, gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, "gateway")))
	remote := dialTest(t, ca, start(t, ca, &Endpoints{Activity: local}), "core").Activity
	auth, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, Kind: serviceapi.PrincipalKindOperator, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanels)})
	if err != nil {
		t.Fatal(err)
	}
	panel, err := local.CreatePanel(ctx, serviceapi.PanelCommand{Auth: auth, CommandID: uuid.NewString(), Name: "Synthetic", EmployeeIDs: []int64{7}, DisplayWindow: serviceapi.DisplayWindow{From: "09:00", To: "18:00"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"empty_name", "empty_employees", "empty_window"} {
		t.Run(name, func(t *testing.T) {
			current, readErr := remote.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: panel.ID})
			if readErr != nil {
				t.Fatal(readErr)
			}
			c := serviceapi.PanelCommand{Auth: auth, PanelID: panel.ID, Revision: current.Revision}
			switch name {
			case "empty_name":
				c.HasName = true
			case "empty_employees":
				c.HasEmployees = true
				c.EmployeeIDs = []int64{}
			case "empty_window":
				c.HasWindow = true
			}
			_, localErr := local.PatchPanel(ctx, c)
			_, remoteErr := remote.PatchPanel(ctx, c)
			if serviceapi.ErrorCode(localErr) != serviceapi.InvalidArgument {
				t.Fatalf("local validation: %v", localErr)
			}
			if serviceapi.ErrorCode(remoteErr) != serviceapi.InvalidArgument {
				t.Fatalf("empty field bypassed validation over mTLS: local=%v remote=%v", localErr, remoteErr)
			}
		})
	}
	renamed, err := remote.PatchPanel(ctx, serviceapi.PanelCommand{Auth: auth, PanelID: panel.ID, Revision: panel.Revision, Name: "Renamed", HasName: true, EmployeeIDs: []int64{7}, HasEmployees: true, DisplayWindow: serviceapi.DisplayWindow{From: "10:00", To: "19:00"}, HasWindow: true})
	if err != nil || renamed.Name != "Renamed" || renamed.DisplayWindow.From != "10:00" {
		t.Fatalf("valid patch not applied: %v", err)
	}
}
