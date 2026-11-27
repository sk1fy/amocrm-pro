package platformlifecycle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	AccountLicensePath         = "/internal/platform/v1/installations/account-license"
	MaxAccountLicenseHintsBody = 1024
	maxAccountLicenseTrialEnd  = 253402300799 // 9999-12-31T23:59:59Z
)

var errInvalidAccountLicenseHints = errors.New("invalid account license hints")

type AccountLicenseHints struct {
	TariffID *int64  `json:"tariff_id,omitempty"`
	PayType  *string `json:"pay_type,omitempty"`
	TrialEnd *int64  `json:"trial_end,omitempty"`
}

func (h AccountLicenseHints) Validate() error {
	if h.TariffID == nil && h.PayType == nil && h.TrialEnd == nil ||
		h.TariffID != nil && (*h.TariffID <= 0 || *h.TariffID > MaxPosition) ||
		h.TrialEnd != nil && (*h.TrialEnd <= 0 || *h.TrialEnd > maxAccountLicenseTrialEnd) {
		return errInvalidAccountLicenseHints
	}
	if h.PayType != nil {
		payType := *h.PayType
		if payType == "" || len(payType) > 64 || strings.TrimSpace(payType) != payType ||
			!utf8.ValidString(payType) || strings.ContainsFunc(payType, unicode.IsControl) {
			return errInvalidAccountLicenseHints
		}
	}
	return nil
}

// UnmarshalJSON requires exactly the named hints and rejects duplicate and null fields.
func (h *AccountLicenseHints) UnmarshalJSON(raw []byte) error {
	if len(raw) > MaxAccountLicenseHintsBody || !utf8.Valid(raw) {
		return errInvalidAccountLicenseHints
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return errInvalidAccountLicenseHints
	}
	var decoded AccountLicenseHints
	fields := map[string]any{
		"tariff_id": &decoded.TariffID,
		"pay_type":  &decoded.PayType,
		"trial_end": &decoded.TrialEnd,
	}
	for decoder.More() {
		key, err := decoder.Token()
		name, isString := key.(string)
		target, allowed := fields[name]
		if err != nil || !isString || !allowed {
			return errInvalidAccountLicenseHints
		}
		delete(fields, name)
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(value, []byte("null")) || json.Unmarshal(value, target) != nil {
			return errInvalidAccountLicenseHints
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return errInvalidAccountLicenseHints
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidAccountLicenseHints
	}
	if err := decoded.Validate(); err != nil {
		return err
	}
	*h = decoded
	return nil
}

func DecodeAccountLicenseHints(raw []byte) (AccountLicenseHints, error) {
	var hints AccountLicenseHints
	err := Decode(raw, &hints)
	return hints, err
}

type AccountLicenseRequest struct {
	Scope      ActivationScope     `json:"scope"`
	ReceivedAt time.Time           `json:"received_at"`
	Hints      AccountLicenseHints `json:"hints"`
}

func (r AccountLicenseRequest) Validate() error {
	scope := r.Scope
	if scope.Installation == uuid.Nil || scope.Integration == uuid.Nil || scope.Account <= 0 ||
		scope.Generation <= 0 || scope.AuthorizationSequence < scope.Generation ||
		scope.AuthorizationSequence > MaxPosition || r.ReceivedAt.Unix() <= 0 {
		return errors.New("invalid account license request")
	}
	return r.Hints.Validate()
}

func DecodeAccountLicenseRequest(raw []byte) (AccountLicenseRequest, error) {
	var request AccountLicenseRequest
	if err := Decode(raw, &request); err != nil {
		return request, err
	}
	return request, request.Validate()
}
