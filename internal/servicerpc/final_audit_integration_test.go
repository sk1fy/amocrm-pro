package servicerpc

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	product "github.com/sk1fy/amocrm-pro/internal/services/activity"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

type finalAuditAPI struct{ stage2ReadAPI }

func (a finalAuditAPI) ListCustomFields(context.Context, uuid.UUID, string) ([]amocrm.CustomField, error) {
	return []amocrm.CustomField{{ID: 10, Name: "Цвет", EntityType: "leads", Enums: []amocrm.CustomFieldEnum{{ID: 1, Value: "Новое имя"}}}, {ID: 20, Name: "Размер", EntityType: "leads", Enums: []amocrm.CustomFieldEnum{{ID: 1, Value: "Большой"}}}}, nil
}

func TestFinalAuditOwnerRPCPublicCards(t *testing.T) {
	f := newCRMParity(t)
	ctx := context.Background()
	ca := newCA(t)
	at := time.Now().Add(-time.Minute).Unix()
	api := finalAuditAPI{stage2ReadAPI{dir: amocrm.AccountDirectory{Timezone: "UTC", Users: []amocrm.DirectoryUser{{ID: 7, Name: "Employee"}}}}}
	add := func(id, kind, before, after string) {
		api.events = append(api.events, amocrm.CRMEvent{ID: id, Type: kind, CreatedAt: at, CreatedBy: 7, EntityType: "lead", EntityID: 31, LinkedTalkContactID: 32, ValueBefore: json.RawMessage(before), ValueAfter: json.RawMessage(after)})
	}
	add("budget", "sale_field_changed", `[{"sale_field_value":{"sale":1000}}]`, `[{"sale_field_value":{"sale":0}}]`)
	add("enum", "custom_field_value_changed", `[{"custom_field_value":{"field_id":10,"enum_id":1,"text":"Старое имя"}}]`, `[{"custom_field_value":{"field_id":10,"enum_id":2}},{"custom_field_value":{"field_id":20,"enum_id":1}}]`)
	add("incoming", "incoming_chat_message", `[]`, `[{"message":{"id":"msg-in"}}]`)
	add("outgoing", "outgoing_chat_message", `[]`, `[{"message":{"id":"msg-out","text":"Текст из payload","source":"test-channel"}}]`)
	add("removed", "custom_field_value_changed", `[{"custom_field_value":{"field_id":10,"enum_id":1}}]`, `null`)
	add("note", "common_note_added", `[]`, `[{"note":{"id":101}}]`)
	add("note", "common_note_added", `[]`, `[{"note":{"id":202}}]`)
	gw := gateway.New(api, corepolicy.ForCaller(f.policy, serviceapi.GatewayService))
	gwAddr := start(t, ca, &Endpoints{Gateway: gw})
	cfg := crmevents.DefaultConfig()
	cfg.Window = 24 * time.Hour
	owner := crmevents.New(f.pool, corepolicy.ForCaller(f.policy, serviceapi.EventsService), dialTest(t, ca, gwAddr, serviceapi.EventsService).Gateway, cfg)
	if _, err := owner.Apply(ctx, serviceapi.Command{Auth: f.auth, CommandID: uuid.NewString(), Kind: "sync", InitialDays: 1, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if worked, err := owner.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("collect %v %v", worked, err)
		}
	}
	for i := 0; i < 20; i++ {
		worked, err := owner.EnrichOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
		if i == 19 {
			t.Fatal("enrichment did not drain")
		}
	}
	ownerAddr := startCRMParity(t, ca, owner)
	ownerRPC := dialTest(t, ca, ownerAddr, serviceapi.ActivityService).CRMEvents
	repo := &parityRepository{settings: serviceapi.DefaultSettings(), commands: map[string]serviceapi.SettingsCommand{}}
	remoteProduct := product.New(repo, corepolicy.ForCaller(f.policy, serviceapi.ActivityService), ownerRPC, dialTest(t, ca, gwAddr, serviceapi.ActivityService).Gateway)
	remote := dialTest(t, ca, start(t, ca, &Endpoints{Activity: remoteProduct}), serviceapi.CoreService).Activity
	principal := widgetauth.Principal{IntegrationID: f.check.primary.IntegrationID, InstallationID: f.check.primary.InstallationID, AccountID: 42, UserID: 7}
	handler := stage2HTTP(t, activitybridge.New(nil, corepolicy.ForCaller(f.policy, serviceapi.CoreService), remote, dialTest(t, ca, ownerAddr, serviceapi.CoreService).CRMEvents), principal)
	expected := map[string][]string{"budget": {"1000 → 0"}, "enum": {"Новое имя (#1)", "Большой (#1)", "#2 (название варианта недоступно)", "Старое имя"}, "incoming": {"Входящее сообщение", "msg-in", "Текст отсутствует", "32"}, "outgoing": {"Исходящее сообщение", "msg-out", "Текст из payload", "test-channel"}, "removed": {"Значение: null", "Новое имя (#1)"}, "note": {"202"}}
	cards := map[string]serviceapi.Event{}
	for id, wants := range expected {
		t.Run(id, func(t *testing.T) {
			var got serviceapi.Event
			stage2Request(t, handler, "/api/v1/widget/activity/events/"+id, http.StatusOK, &got)
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range wants {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("missing %q in %s", want, raw)
				}
			}
			if id == "enum" {
				n := 0
				for _, d := range got.View.Details {
					if strings.HasPrefix(d.Key, "custom_field_enum:") {
						n++
						if !d.Current || d.Source != serviceapi.SourceCustomFieldsAPI {
							t.Fatal(d)
						}
					}
				}
				if n != 3 {
					t.Fatalf("enum labels=%d", n)
				}
			}
			if id == "note" {
				for _, o := range got.Enrichment {
					if o.ObjectKind == "note" && o.ObjectKey != "202" {
						t.Fatalf("stale note %+v", o)
					}
				}
			}
			if id == "removed" && string(got.ValueAfter) != "null" {
				t.Fatal("null lost")
			}
			cards[id] = got
		})
	}
	if path := os.Getenv("ACTIVITY_UI_CARDS_FIXTURE"); path != "" {
		raw, err := json.Marshal(cards)
		if err != nil {
			t.Fatal(err)
		}
		// These synthetic cards contain no credentials. The host Actions runner
		// must be able to upload the fixture written by the test container.
		if err = os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		// WriteFile preserves an existing mode, including old 0600 fixtures.
		if err = os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	}
}
