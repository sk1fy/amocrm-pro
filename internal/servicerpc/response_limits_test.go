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
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
	"strings"
	"testing"
)

type responseRepository struct {
	crmevents.Repository
	result serviceapi.QueryResult
}

func (r responseRepository) Query(context.Context, serviceapi.Query, serviceapi.Principal) (serviceapi.QueryResult, error) {
	return r.result, nil
}

type responseEvents struct {
	serviceapi.CRMEvents
	result serviceapi.QueryResult
}

func (e responseEvents) Query(context.Context, serviceapi.Query) (serviceapi.QueryResult, error) {
	return e.result, nil
}
func (e responseEvents) Status(context.Context, serviceapi.Auth) (serviceapi.SyncStatus, error) {
	return e.result.Status, nil
}

func TestLargeOwnerResponsesHaveSameLocalAndGRPCError(t *testing.T) {
	ctx := context.Background()
	ca := newCA(t)
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	policy, err := corepolicy.NewWithChecker(&checker{scope: scope}, key)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := corepolicy.ForCaller(policy, "core").Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: "activity", RequestID: "large-response", Grants: serviceapi.UserGrants()})
	if err != nil {
		t.Fatal(err)
	}
	// Individually valid retained payloads can exceed a page's total byte budget.
	encoded, _ := json.Marshal([]map[string]string{{"value": strings.Repeat("x", 32750)}})
	raw := json.RawMessage(encoded)
	large := serviceapi.QueryResult{}
	for i := 0; i < 100; i++ {
		large.Events = append(large.Events, serviceapi.Event{ID: uuid.NewString(), CreatedAt: 100, CreatedBy: 7, ValueBefore: raw, ValueAfter: raw})
	}
	eventOwner := crmevents.NewWithRepository(responseRepository{result: large}, corepolicy.ForCaller(policy, "crm-events"), nil, crmevents.DefaultConfig())
	// Activity's own envelope guard is tested separately with an unguarded dependency.
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	product := activity.New(repo, corepolicy.ForCaller(policy, "activity"), responseEvents{result: large}, gateway.New(&fakeAPI{}, corepolicy.ForCaller(policy, "gateway")))
	remote := dialTest(t, ca, start(t, ca, &Endpoints{CRMEvents: eventOwner, Activity: product}), "core")
	q := serviceapi.Query{Auth: auth, From: 90, To: 200, UserIDs: []int64{7}, Limit: 100}
	var want string
	for _, call := range []func() error{func() error { _, err := eventOwner.Query(ctx, q); return err }, func() error { _, err := remote.CRMEvents.Query(ctx, q); return err }, func() error { _, err := product.Panel(ctx, q); return err }, func() error { _, err := remote.Activity.Panel(ctx, q); return err }} {
		err := call()
		if serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted || !strings.Contains(err.Error(), "smaller page") {
			t.Fatalf("large response=%v", err)
		}
		if want == "" {
			want = err.Error()
		}
		if err.Error() != want {
			t.Fatalf("local/grpc size errors differ: %q / %q", want, err)
		}
	}
}
