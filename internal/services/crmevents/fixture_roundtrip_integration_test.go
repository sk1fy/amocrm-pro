package crmevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/gateway"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// A fixture-only transport exercises the production HTTP decoder and Gateway
// conversion without sending synthetic events or credentials to an account.
type fixtureEventTransport func(*http.Request) (*http.Response, error)

func (f fixtureEventTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixtureEventTokens struct{ scope serviceapi.Scope }

func (f fixtureEventTokens) Token(context.Context, uuid.UUID) (amocrm.AccessToken, error) {
	return amocrm.AccessToken{InstallationID: f.scope.InstallationID, IntegrationID: f.scope.IntegrationID, AccountID: 999001, AccountDomain: "synthetic.amocrm.ru", Value: "synthetic-test-only"}, nil
}
func (fixtureEventTokens) RefreshIfCurrent(context.Context, amocrm.AccessToken) (amocrm.AccessToken, error) {
	return amocrm.AccessToken{}, errors.New("fixture must not refresh tokens")
}
func (fixtureEventTokens) MarkReauthRequired(context.Context, uuid.UUID, int64) error {
	return errors.New("fixture must not mark reauthorization")
}

func fixtureJSONValue(t *testing.T, payload json.RawMessage) any {
	t.Helper()
	// Existing normalization is intentional: missing payload becomes [], while
	// explicit JSON null remains null. Number text never goes through float64.
	if len(payload) == 0 {
		payload = json.RawMessage(`[]`)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestFixtureHTTPGatewayJSONBRoundTrip(t *testing.T) {
	data, err := os.ReadFile("../../../docs/fixtures/activity-events-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Period struct{ From, To int64 }
		Cases  []struct {
			ID    string          `json:"case_id"`
			Input json.RawMessage `json:"input"`
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 29 {
		t.Fatalf("fixture coverage changed: got %d cases", len(fixture.Cases))
	}
	inputs := make([]json.RawMessage, 0, len(fixture.Cases))
	for _, entry := range fixture.Cases {
		inputs = append(inputs, entry.Input)
	}
	body, err := json.Marshal(map[string]any{"_embedded": map[string]any{"events": inputs}})
	if err != nil {
		t.Fatal(err)
	}
	s, p, _ := setup(t)
	store := testStore(s)
	requestCount := 0
	client := amocrm.NewClient(&http.Client{Transport: fixtureEventTransport(func(r *http.Request) (*http.Response, error) {
		requestCount++
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/events" || q.Get("filter[created_at][from]") != fmt.Sprint(fixture.Period.From) || q.Get("filter[created_at][to]") != fmt.Sprint(fixture.Period.To) || q.Get("page") != "1" || q.Get("limit") != "100" {
			t.Errorf("unexpected fixture request: %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})}, fixtureEventTokens{scope: p.principal.Scope})
	s.gateway = gateway.New(client, p)
	op := accepted(t, s, p)
	_, err = store.pool.Exec(context.Background(), `UPDATE event_jobs SET window_from=to_timestamp($2),window_to=to_timestamp($3),target_to=to_timestamp($3) WHERE installation_id=$1`, p.principal.InstallationID, fixture.Period.From, fixture.Period.To)
	if err != nil {
		t.Fatal(err)
	}
	runPages(t, s, 2)
	if requestCount != 2 {
		t.Fatalf("stabilized scan requests=%d", requestCount)
	}
	actual := opState(t, s, op.ID)
	if actual.State != serviceapi.OperationSucceeded || actual.Inserted != 29 || actual.Deduplicated != 29 || actual.Updated != 0 {
		t.Fatalf("fixture counters=%+v", actual)
	}
	q := serviceapi.Query{From: fixture.Period.From, To: fixture.Period.To, Limit: 100}
	result, err := s.Query(context.Background(), q)
	if err != nil || len(result.Events) != 29 || result.NextCursor != "" {
		t.Fatalf("fixture query count=%d cursor=%q err=%v", len(result.Events), result.NextCursor, err)
	}
	byID := make(map[string]serviceapi.Event, len(result.Events))
	for _, event := range result.Events {
		byID[event.ID] = event
	}
	for _, entry := range fixture.Cases {
		t.Run(entry.ID, func(t *testing.T) {
			var expected serviceapi.Event
			if err := json.Unmarshal(entry.Input, &expected); err != nil {
				t.Fatal(err)
			}
			got, ok := byID[expected.ID]
			if !ok {
				t.Fatal("event was omitted")
			}
			if !reflect.DeepEqual(fixtureJSONValue(t, expected.ValueBefore), fixtureJSONValue(t, got.ValueBefore)) || !reflect.DeepEqual(fixtureJSONValue(t, expected.ValueAfter), fixtureJSONValue(t, got.ValueAfter)) {
				t.Fatal("JSONB round-trip changed before/after contents")
			}
			expected.ValueBefore, expected.ValueAfter = nil, nil
			got.ValueBefore, got.ValueAfter = nil, nil
			if !reflect.DeepEqual(expected, got) {
				t.Fatalf("typed envelope changed: got=%+v want=%+v", got, expected)
			}
		})
	}
	if byID["synthetic-chat_reference"].LinkedTalkContactID != 32001 {
		t.Fatal("linked chat contact lost")
	}
	if result.Status.VerifiedFrom != fixture.Period.From || result.Status.VerifiedThrough != fixture.Period.To {
		t.Fatalf("stable fixture coverage=%+v", result.Status)
	}
}

func TestEventPayloadBoundsRemainAtomicAtOwner(t *testing.T) {
	exact := json.RawMessage(`"` + strings.Repeat("x", 32766) + `"`)
	over := json.RawMessage(`"` + strings.Repeat("x", 32767) + `"`)
	for _, tc := range []struct {
		name          string
		before, after json.RawMessage
		linked        int64
		accept        bool
	}{
		{"exact_both", exact, exact, math.MaxInt64, true},
		{"oversize_before", over, json.RawMessage(`[]`), 0, false},
		{"oversize_after", json.RawMessage(`[]`), over, 0, false},
		{"negative_contact", nil, nil, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p, g := setup(t)
			op := accepted(t, s, p)
			at := time.Now().Add(-time.Minute).Unix()
			g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
				return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "valid", CreatedAt: at, CreatedBy: 7}, {ID: "bounded", CreatedAt: at, CreatedBy: 7, ValueBefore: tc.before, ValueAfter: tc.after, LinkedTalkContactID: tc.linked}}}, nil
			}
			runPages(t, s, 1)
			if tc.accept {
				runPages(t, s, 1)
			}
			var count, coverage int
			if err := testStore(s).pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM crm_events WHERE installation_id=$1),(SELECT count(*) FROM event_coverage WHERE installation_id=$1)`, p.principal.InstallationID).Scan(&count, &coverage); err != nil {
				t.Fatal(err)
			}
			actual := opState(t, s, op.ID)
			if tc.accept {
				if count != 2 || coverage != 1 || actual.State != serviceapi.OperationSucceeded || actual.Inserted != 2 || actual.Deduplicated != 2 {
					t.Fatalf("bounded page rejected: count=%d coverage=%d operation=%+v", count, coverage, actual)
				}
				var before, after json.RawMessage
				var contact int64
				if err := testStore(s).pool.QueryRow(context.Background(), `SELECT value_before,value_after,linked_talk_contact_id FROM crm_events WHERE installation_id=$1 AND event_id='bounded'`, p.principal.InstallationID).Scan(&before, &after, &contact); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, exact) || !bytes.Equal(after, exact) || contact != math.MaxInt64 {
					t.Fatal("maximum accepted payload was truncated or contact rounded")
				}
			} else if count != 0 || coverage != 0 || actual.State != serviceapi.OperationFailed || actual.Inserted != 0 || actual.Processed != 0 {
				t.Fatalf("invalid page was partially saved or covered: count=%d coverage=%d operation=%+v", count, coverage, actual)
			}
		})
	}
}

func TestLinkedTalkContactChangeUpdatesOneEventAndDigest(t *testing.T) {
	s, p, g := setup(t)
	op := accepted(t, s, p)
	var linked int64
	at := time.Now().Add(-time.Minute).Unix()
	g.events = func(serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
		return serviceapi.EventPage{Events: []serviceapi.Event{{ID: "chat", CreatedAt: at, CreatedBy: 7, Type: "incoming_chat_message", EntityID: 8, EntityType: "lead", LinkedTalkContactID: linked}}}, nil
	}
	runPages(t, s, 1)
	linked = math.MaxInt64
	runPages(t, s, 1)
	// A newly discovered optional reference is a mutation, requiring another
	// stable pass; it must not create a second event or count as a duplicate.
	actual := opState(t, s, op.ID)
	if actual.State == serviceapi.OperationSucceeded || actual.Inserted != 1 || actual.Updated != 1 || actual.Deduplicated != 0 {
		t.Fatalf("contact change not detected: %+v", actual)
	}
	runPages(t, s, 1)
	actual = opState(t, s, op.ID)
	if actual.State != serviceapi.OperationSucceeded || actual.Inserted != 1 || actual.Updated != 1 || actual.Deduplicated != 1 {
		t.Fatalf("stable contact replay: %+v", actual)
	}
	result, err := s.Query(context.Background(), serviceapi.Query{From: at - 1, To: at + 1, Limit: 100})
	if err != nil || len(result.Events) != 1 || result.Events[0].LinkedTalkContactID != linked {
		t.Fatalf("stored reference count=%d err=%v", len(result.Events), err)
	}
}

func TestCanonicalEventAbsentContactRetainsLegacyHashBytes(t *testing.T) {
	// Adding omitempty keeps absent new fields from marking every old row as
	// changed after upgrade. Keep a literal legacy envelope as independent proof.
	e := serviceapi.Event{ID: "legacy", CreatedAt: 1, CreatedBy: 7, Type: "lead_added", EntityID: 8, EntityType: "lead"}
	_, encoded, err := canonicalEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	const legacy = `{"id":"legacy","created_at":1,"created_by":7,"type":"lead_added","entity_id":8,"entity_type":"lead","value_before":[],"value_after":[]}`
	if string(encoded) != legacy {
		t.Fatalf("legacy content hash bytes changed: %s", encoded)
	}
}
