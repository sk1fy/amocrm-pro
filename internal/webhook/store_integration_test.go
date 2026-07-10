package webhook

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/testkit"
)

func TestDeliverySnapshotsSettingsAllowsRepeatedRequestIDAndRetainsDedup(t *testing.T) {
	pool := testkit.Postgres(t)
	testkit.Reset(t, pool)
	ctx := context.Background()
	integrationID := uuid.New()
	installationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO integrations (
			id, code, client_id, client_secret_ciphertext, redirect_uri, webhook_events
		) VALUES ($1, 'integration-test', $2, decode('00','hex'),
			'https://example.test/oauth', '["update_lead"]'::jsonb)`,
		integrationID, uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO installations (
			id, integration_id, account_id, account_domain, status,
			webhook_status, webhook_settings
		) VALUES ($1, $2, 42, 'tenant.amocrm.ru', 'active', 'active', '["update_lead"]'::jsonb)`,
		installationID, integrationID); err != nil {
		t.Fatal(err)
	}

	store := NewStore(pool)
	requestID := uuid.New()
	raw := []byte("account[id]=42&leads[update][0][id]=100&leads[update][0][last_modified]=1710000000")
	firstID, err := store.SaveDeliveryAndEnqueue(ctx, installationID, requestID, "application/x-www-form-urlencoded", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE installations SET webhook_settings='[]'::jsonb WHERE id=$1`, installationID); err != nil {
		t.Fatal(err)
	}
	secondID, err := store.SaveDeliveryAndEnqueue(ctx, installationID, requestID, "application/x-www-form-urlencoded", raw)
	if err != nil {
		t.Fatalf("repeated request id must remain correlation-only: %v", err)
	}

	first, err := store.GetDelivery(ctx, firstID, installationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.WebhookEvents) != 1 || first.WebhookEvents[0] != "update_lead" {
		t.Fatalf("receipt-time settings were not preserved: %#v", first.WebhookEvents)
	}
	second, err := store.GetDelivery(ctx, secondID, installationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.WebhookEvents) != 0 {
		t.Fatalf("new settings snapshot not used: %#v", second.WebhookEvents)
	}
	if _, err := store.GetDelivery(ctx, firstID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delivery lookup should fail, got %v", err)
	}

	events, err := ParseAllowed(installationID, first.RawBody, first.WebhookEvents)
	if err != nil || len(events) != 1 {
		t.Fatalf("parse snapshot: events=%d err=%v", len(events), err)
	}
	inserted, err := store.SaveParsedEvents(ctx, first, events)
	if err != nil || inserted != 1 {
		t.Fatalf("save first events: inserted=%d err=%v", inserted, err)
	}
	secondEvents, err := ParseAllowed(installationID, second.RawBody, second.WebhookEvents)
	if err != nil || len(secondEvents) != 0 {
		t.Fatalf("empty allowlist should ignore events: events=%d err=%v", len(secondEvents), err)
	}
	if inserted, err := store.SaveParsedEvents(ctx, second, secondEvents); err != nil || inserted != 0 {
		t.Fatalf("save ignored delivery: inserted=%d err=%v", inserted, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id=$1`, firstID); err == nil {
		t.Fatal("delivery deletion must not cascade away retained deduplication events")
	}
}
