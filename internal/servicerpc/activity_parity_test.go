package servicerpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"reflect"
	"sync"
	"testing"
)

type parityRepository struct {
	mu       sync.Mutex
	settings serviceapi.Settings
	commands map[string]serviceapi.SettingsCommand
}

func (r *parityRepository) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings, nil
}
func (r *parityRepository) Configure(_ context.Context, _ serviceapi.Principal, c serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.commands[c.CommandID]; ok && old.Settings != c.Settings {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.Conflict, "different settings")
	}
	r.commands[c.CommandID] = c
	r.settings = c.Settings
	return serviceapi.Operation{ID: c.CommandID, CommandID: c.CommandID, State: "succeeded"}, nil
}
func (r *parityRepository) Operation(_ context.Context, _ serviceapi.Principal, id string) (serviceapi.Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.commands[id]; !ok {
		return serviceapi.Operation{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return serviceapi.Operation{ID: id, CommandID: id, State: "succeeded"}, nil
}

type parityEvents struct {
	serviceapi.CRMEvents
	policy serviceapi.Policy
}

func (e *parityEvents) Query(ctx context.Context, q serviceapi.Query) (serviceapi.QueryResult, error) {
	if _, err := e.policy.Validate(ctx, q.Auth, "crm-events", "read"); err != nil {
		return serviceapi.QueryResult{}, err
	}
	return serviceapi.QueryResult{Events: []serviceapi.Event{{ID: "event", CreatedAt: 100, CreatedBy: 7, Type: "lead_added", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)}}, Summaries: []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1, LastEventAt: 100}}, Status: serviceapi.SyncStatus{State: "not_configured"}}, nil
}

func TestActivityBusinessLocalAndMTLSGRPCParity(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &checker{scope: scope}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	events := &parityEvents{policy: corepolicy.ForCaller(policy, "crm-events")}
	gw := gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, "gateway"))
	local := product.New(repo, corepolicy.ForCaller(policy, "activity"), events, gw)
	address := start(t, ca, &Endpoints{Activity: local})
	remote := dialTest(t, ca, address, "core").Activity
	auth, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: "activity", RequestID: uuid.NewString(), Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	query := serviceapi.Query{Auth: auth, From: 90, To: 200, UserIDs: []int64{7}, Limit: 100}
	localPanel, err := local.Panel(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	remotePanel, err := remote.Panel(ctx, query)
	if err != nil || !reflect.DeepEqual(localPanel, remotePanel) {
		t.Fatalf("local=%+v remote=%+v err=%v", localPanel, remotePanel, err)
	}
	if remotePanel.Coverage != "unknown" || len(remotePanel.Data.Events) != 1 || remotePanel.Data.Summaries[0].UniqueEvents != 1 {
		t.Fatalf("incorrect useful panel: %+v", remotePanel)
	}
	command := serviceapi.SettingsCommand{Auth: auth, CommandID: uuid.NewString(), Settings: serviceapi.Settings{InitialDays: 3, RetentionDays: 10}}
	accepted, err := remote.Configure(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := local.Configure(ctx, command)
	if err != nil || accepted != replay {
		t.Fatalf("local replay after remote commit=%+v err=%v", replay, err)
	}
	for _, client := range []serviceapi.Activity{local, remote} {
		settings, err := client.Settings(ctx, auth)
		if err != nil || settings != command.Settings {
			t.Fatalf("settings=%+v err=%v", settings, err)
		}
		op, err := client.Operation(ctx, serviceapi.OperationRequest{Auth: auth, OperationID: command.CommandID})
		if err != nil || op != accepted {
			t.Fatalf("operation=%+v err=%v", op, err)
		}
		bad := command
		bad.Settings.RetentionDays++
		if _, err := client.Configure(ctx, bad); serviceapi.ErrorCode(err) != serviceapi.Conflict {
			t.Fatalf("changed command=%v", err)
		}
		invalid := query
		invalid.To = invalid.From + 32*86400
		if _, err := client.Panel(ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.InvalidArgument {
			t.Fatalf("query bound=%v", err)
		}
		invalid = query
		invalid.Auth.Token = "forged"
		if _, err := client.Panel(ctx, invalid); serviceapi.ErrorCode(err) != serviceapi.Unauthenticated {
			t.Fatalf("forgery=%v", err)
		}
	}
	check.disabled.Store(true)
	for _, client := range []serviceapi.Activity{local, remote} {
		if _, err := client.Panel(ctx, query); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
			t.Fatalf("disabled local/remote=%v", err)
		}
	}
}
