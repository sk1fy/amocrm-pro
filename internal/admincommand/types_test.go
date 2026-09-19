package admincommand

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
)

func TestCheckClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		want  string
		retry int64
	}{
		{nil, "verified_ok", 0},
		{&amocrm.APIError{Kind: amocrm.ErrorUnauthorized}, "auth_error", 0},
		{&amocrm.APIError{Kind: amocrm.ErrorForbidden}, "internal_error", 0},
		{&amocrm.APIError{Kind: amocrm.ErrorValidation, StatusCode: 400}, "internal_error", 0},
		{&amocrm.APIError{Kind: amocrm.ErrorValidation, StatusCode: 422}, "internal_error", 0},
		{&amocrm.APIError{Kind: amocrm.ErrorRateLimited, RetryAfter: 9 * time.Second}, "rate_limited", 9},
		{context.DeadlineExceeded, "network_error", 0},
		{&net.DNSError{Err: "fixture", IsTimeout: true}, "network_error", 0},
		{errors.New("internal fixture"), "internal_error", 0},
		{oauth.ErrRefreshOutcomeUnknown, "internal_error", 0},
	} {
		got, retry := classifyCheck(tc.err)
		if got != tc.want || retry != tc.retry {
			t.Fatalf("classification=%s/%d want %s/%d", got, retry, tc.want, tc.retry)
		}
	}
}

func TestReceiptIDAndCanonicalReplayHash(t *testing.T) {
	id := uuid.New()
	if ReceiptID(id.String()) != id {
		t.Fatal("UUID key must be receipt ID")
	}
	if ReceiptID("fixture-key") != uuid.NewSHA1(uuid.NameSpaceOID, []byte("fixture-key")) {
		t.Fatal("non UUID key must use UUIDv5 OID")
	}
	req := request("integration", id, "set-service")
	req.Payload = []byte(`{"service":"activity","enabled":false}`)
	_, _, _, first, err := normalize(req, "fixture-key", fixtureActor)
	if err != nil {
		t.Fatal(err)
	}
	req.Payload = []byte(`{ "enabled":false, "service":"activity" }`)
	_, _, _, second, err := normalize(req, "fixture-key", "employee:other")
	if err != nil || first != second {
		t.Fatal("semantic JSON replay hash changed")
	}
	req.Payload = []byte(`{"service":"activity","enabled":true}`)
	_, _, _, third, err := normalize(req, "fixture-key", fixtureActor)
	if err != nil || first == third {
		t.Fatal("changed payload hash was accepted")
	}
}
