package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type fakeWebhookGateway struct {
	listed      []amocrm.Webhook
	listErr     error
	deleted     []string
	deleteErr   error
	registered  []amocrm.WebhookSpec
	registerErr error
}

func (f *fakeWebhookGateway) ListWebhooks(context.Context, uuid.UUID, string) ([]amocrm.Webhook, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]amocrm.Webhook(nil), f.listed...), nil
}

func (f *fakeWebhookGateway) RegisterWebhook(_ context.Context, _ uuid.UUID, spec amocrm.WebhookSpec) (amocrm.Webhook, error) {
	if f.registerErr != nil {
		return amocrm.Webhook{}, f.registerErr
	}
	f.registered = append(f.registered, spec)
	return amocrm.Webhook{Destination: spec.Destination, Settings: spec.Settings}, nil
}

func (f *fakeWebhookGateway) DeleteWebhook(_ context.Context, _ uuid.UUID, destination string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, destination)
	var remaining []amocrm.Webhook
	for _, hook := range f.listed {
		if hook.Destination != destination {
			remaining = append(remaining, hook)
		}
	}
	f.listed = remaining
	return nil
}

func TestPlanManagedWebhooksDeletesStaleDuplicatesAndKeepsDesired(t *testing.T) {
	desired := "https://widgets.example.test/hooks/amocrm/v1/current"
	settings := []string{"add_lead", "update_lead"}
	actual := []amocrm.Webhook{
		{ID: 1, Destination: desired, Settings: settings},
		{ID: 2, Destination: "https://widgets.example.test/hooks/amocrm/v1/stale", Settings: settings},
		{ID: 3, Destination: "https://old.example.test/hooks/amocrm/v1/current", Settings: settings},
	}
	plan := planManagedWebhooks(desired, settings, actual, false, "https://widgets.example.test/hooks/amocrm/v1/stale", "https://old.example.test/hooks/amocrm/v1/current")
	if plan.register {
		t.Fatal("desired destination should be kept")
	}
	if len(plan.delete) != 2 || plan.delete[0] != "https://old.example.test/hooks/amocrm/v1/current" || plan.delete[1] != "https://widgets.example.test/hooks/amocrm/v1/stale" {
		t.Fatalf("unexpected stale destinations: %#v", plan.delete)
	}
}

func TestPlanManagedWebhooksRotatesDuplicateDesiredDestination(t *testing.T) {
	desired := "https://widgets.example.test/hooks/amocrm/v1/current"
	settings := []string{"add_lead"}
	actual := []amocrm.Webhook{
		{ID: 1, Destination: desired, Settings: []string{"old_event"}},
		{ID: 2, Destination: desired, Settings: settings, Disabled: true},
	}
	plan := planManagedWebhooks(desired, settings, actual, false)
	if !plan.register || len(plan.delete) != 1 || plan.delete[0] != desired {
		t.Fatalf("expected rotate of desired destination, got %#v", plan)
	}
}

func TestPlanManagedWebhooksUnregisterDeletesOnlyOwnedDestinations(t *testing.T) {
	actual := []amocrm.Webhook{
		{Destination: "https://widgets.example.test/hooks/amocrm/v1/a"},
		{Destination: "https://widgets.example.test/hooks/amocrm/v1/a"},
		{Destination: "https://other.example.test/legacy"},
	}
	plan := planManagedWebhooks("", nil, actual, true, "https://widgets.example.test/hooks/amocrm/v1/a")
	if plan.register || len(plan.delete) != 1 {
		t.Fatalf("unregister should delete unique destinations, got %#v", plan)
	}
}

func TestApplyWebhookPlanUnregisterCallsDeleteWithoutRegister(t *testing.T) {
	gateway := &fakeWebhookGateway{listed: []amocrm.Webhook{
		{Destination: "https://widgets.example.test/hooks/amocrm/v1/stale"},
		{Destination: "https://widgets.example.test/hooks/amocrm/v1/current"},
	}}
	if err := syncInstallationWebhooks(context.Background(), gateway, uuid.New(), "", nil, true, "https://widgets.example.test/hooks/amocrm/v1/stale", "https://widgets.example.test/hooks/amocrm/v1/current"); err != nil {
		t.Fatal(err)
	}
	if len(gateway.registered) != 0 {
		t.Fatalf("unregister registered webhooks: %#v", gateway.registered)
	}
	if len(gateway.deleted) != 2 {
		t.Fatalf("expected two deletes, got %#v", gateway.deleted)
	}
}

func TestDeleteWebhookTreatsNotFoundAsAlreadyRemoved(t *testing.T) {
	err := deleteWebhook(context.Background(), &deleteNotFoundGateway{}, uuid.New(), "https://widgets.example.test/hooks/amocrm/v1/gone")
	if err != nil {
		t.Fatalf("missing remote webhook should be success: %v", err)
	}
}

type deleteNotFoundGateway struct{ fakeWebhookGateway }

func (*deleteNotFoundGateway) DeleteWebhook(context.Context, uuid.UUID, string) error {
	return &amocrm.APIError{Kind: amocrm.ErrorNotFound, StatusCode: 404}
}

func TestApplyWebhookPlanStopsOnRemoteError(t *testing.T) {
	gateway := &fakeWebhookGateway{
		listed:    []amocrm.Webhook{{Destination: "https://widgets.example.test/hooks/amocrm/v1/stale"}},
		deleteErr: errors.New("upstream timeout"),
	}
	if err := syncInstallationWebhooks(context.Background(), gateway, uuid.New(), "https://widgets.example.test/hooks/amocrm/v1/current", []string{"add_lead"}, false, "https://widgets.example.test/hooks/amocrm/v1/stale"); err == nil {
		t.Fatal("expected remote delete failure")
	}
}
