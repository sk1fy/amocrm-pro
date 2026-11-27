package platformlifecycle

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecodeAccountLicenseHints(t *testing.T) {
	for _, body := range []string{
		`{"tariff_id":22000000}`, `{"pay_type":"trial"}`, `{"trial_end":253402300799}`,
		`{"tariff_id":9007199254740991,"pay_type":"blocked","trial_end":1795776000}`,
	} {
		if _, err := DecodeAccountLicenseHints([]byte(body)); err != nil {
			t.Errorf("valid hints %s rejected: %v", body, err)
		}
	}
	for _, body := range []string{
		``, `null`, `[]`, `{}`, `{"tariff_id":0}`, `{"tariff_id":-1}`, `{"tariff_id":22000000.0}`,
		`{"tariff_id":9e5}`, `{"tariff_id":"22000000"}`, `{"tariff_id":9007199254740992}`, `{"tariff_id":9223372036854775808}`,
		`{"trial_end":253402300800}`, `{"trial_end":-1}`, `{"trial_end":0}`, `{"trial_end":1.5}`, `{"trial_end":"1795776000"}`,
		`{"pay_type":true}`, `{"pay_type":""}`, `{"pay_type":" trial"}`, `{"pay_type":"trial\nend"}`,
		`{"pay_type":"` + strings.Repeat("ё", 33) + `"}`, "{\"pay_type\":\"\xff\"}",
		`{"pay_type":"trial","tariff_id":null}`, `{"tariff_id":1,"pay_type":null}`, `{"tariff_id":1,"trial_end":null}`,
		`{"tariff_id":1,"tariff_id":2}`, `{"TARIFF_ID":22000000}`, `{"tariffId":22000000}`,
		`{"tariff_id":22000000,"received_at":"2026-11-25T12:00:00Z"}`, `{"tariff_id":22000000,"amo_account_id":2}`,
		`{"tariff_id":22000000}{}`, `{"tariff_id":22000000`,
	} {
		if hints, err := DecodeAccountLicenseHints([]byte(body)); err == nil {
			t.Errorf("invalid hints %q accepted: %+v", body, hints)
		}
	}
}

func TestAccountLicenseRequestRequiresTrustedScopeAndReceipt(t *testing.T) {
	tariff := int64(22000000)
	request := AccountLicenseRequest{
		Scope: ActivationScope{Installation: uuid.New(), Integration: uuid.New(), Account: 9007199254740993,
			Generation: 2, AuthorizationSequence: 4},
		ReceivedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC),
		Hints:      AccountLicenseHints{TariffID: &tariff},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := request
	invalid.Scope.AuthorizationSequence = 1
	if err := invalid.Validate(); err == nil {
		t.Fatal("accepted authorization sequence from an earlier generation")
	}
	invalid = request
	invalid.ReceivedAt = time.Time{}
	if err := invalid.Validate(); err == nil {
		t.Fatal("accepted missing receipt")
	}
}
