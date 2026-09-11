package activity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/crmevents"
)

type stage8Checker struct {
	scope                 serviceapi.Scope
	disabled, unavailable bool
}

func (c *stage8Checker) Check(_ context.Context, s serviceapi.Scope, actor int64, system bool) error {
	if c.unavailable {
		return serviceapi.Fail(serviceapi.Unavailable, "policy unavailable")
	}
	if c.disabled || s != c.scope || (!system && actor != 7) {
		return serviceapi.Fail(serviceapi.PermissionDenied, "not admitted")
	}
	return nil
}

func expireDelegation(t *testing.T, token string, key ed25519.PrivateKey) serviceapi.Auth {
	t.Helper()
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		t.Fatal(err)
	}
	claims["iat"] = time.Now().Add(-31 * time.Second).Unix()
	claims["nbf"] = claims["iat"]
	claims["exp"] = time.Now().Add(-time.Second).Unix()
	expired, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return serviceapi.Auth{Token: expired}
}

func TestPublicReadsRejectExpiredWrongGrantAndUnavailablePolicy(t *testing.T) {
	ctx := context.Background()
	scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
	check := &stage8Checker{scope: scope}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := corepolicy.NewWithChecker(check, key)
	if err != nil {
		t.Fatal(err)
	}
	core := corepolicy.ForCaller(policy, serviceapi.CoreService)
	owner := crmevents.NewWithRepository(nil, corepolicy.ForCaller(policy, serviceapi.EventsService), nil, crmevents.DefaultConfig())
	product := New(&memorySettings{}, corepolicy.ForCaller(policy, serviceapi.ActivityService), owner, &fakeGateway{})
	issue := func(requestID string, grants []serviceapi.Grant) serviceapi.Auth {
		t.Helper()
		auth, err := core.Issue(ctx, serviceapi.IssueRequest{Scope: scope, ActorID: 7, Consumer: serviceapi.ActivityService, RequestID: requestID, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		return auth
	}
	panelAuth := issue("panel", serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionPanel))
	settingsAuth := issue("settings", serviceapi.UserGrantsFor(serviceapi.ActivityService, serviceapi.ActionSettings))
	compact := serviceapi.Query{Auth: panelAuth, From: 100, To: 200, Compact: true, UserIDs: []int64{7}}
	card := serviceapi.EventRequest{Auth: panelAuth, EventID: "card"}
	detail := serviceapi.EventRequest{Auth: panelAuth, EventID: "card"}

	assertDenied := func(name string, code serviceapi.Code, panelQ serviceapi.Query, cardReq, detailReq serviceapi.EventRequest) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if _, err := product.Panel(ctx, panelQ); serviceapi.ErrorCode(err) != code {
				t.Fatalf("compact panel: %v", err)
			}
			if _, err := product.EventCard(ctx, cardReq); serviceapi.ErrorCode(err) != code {
				t.Fatalf("event card: %v", err)
			}
			if _, err := owner.GetEvent(ctx, detailReq); serviceapi.ErrorCode(err) != code {
				t.Fatalf("owner detail: %v", err)
			}
		})
	}

	wrongPanel, wrongCard, wrongDetail := compact, card, detail
	wrongPanel.Auth, wrongCard.Auth, wrongDetail.Auth = settingsAuth, settingsAuth, settingsAuth
	assertDenied("wrong grant", serviceapi.PermissionDenied, wrongPanel, wrongCard, wrongDetail)

	expired := expireDelegation(t, panelAuth.Token, key)
	expiredPanel, expiredCard, expiredDetail := compact, card, detail
	expiredPanel.Auth, expiredCard.Auth, expiredDetail.Auth = expired, expired, expired
	assertDenied("expired delegation", serviceapi.Unauthenticated, expiredPanel, expiredCard, expiredDetail)

	check.unavailable = true
	assertDenied("core unavailable", serviceapi.Unavailable, compact, card, detail)
	check.unavailable = false
}

func TestEventCardMapsOversizedResponseLikeQuery(t *testing.T) {
	payload := json.RawMessage(`"` + strings.Repeat("x", serviceapi.MaxResponseBytes) + `"`)
	event := serviceapi.Event{ID: "huge", CreatedAt: 150, CreatedBy: 7, Type: "common_note_added", ValueAfter: payload}
	events := &fakeEvents{
		event: event,
		result: serviceapi.QueryResult{
			ReadVersion: serviceapi.PresentationReadVersion,
			Events:      []serviceapi.Event{event},
			Summaries:   []serviceapi.UserSummary{{UserID: 7, UniqueEvents: 1}},
		},
	}
	s := New(&memorySettings{}, &fakePolicy{}, events, &fakeGateway{})
	_, cardErr := s.EventCard(context.Background(), serviceapi.EventRequest{Auth: serviceapi.Auth{Token: "verified"}, EventID: event.ID})
	_, panelErr := s.Panel(context.Background(), serviceapi.Query{Auth: serviceapi.Auth{Token: "verified"}, From: 100, To: 200})
	if serviceapi.ErrorCode(cardErr) != serviceapi.ResourceExhausted || serviceapi.ErrorCode(panelErr) != serviceapi.ResourceExhausted {
		t.Fatalf("card=%v panel=%v", cardErr, panelErr)
	}
	if !strings.Contains(cardErr.Error(), "smaller page") || cardErr.Error() != panelErr.Error() {
		t.Fatalf("error mapping differs card=%q panel=%q", cardErr, panelErr)
	}
}
