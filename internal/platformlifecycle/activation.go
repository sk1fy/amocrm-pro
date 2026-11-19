package platformlifecycle

import (
	"errors"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const ActivationPath = "/internal/platform/v1/activation"
const ContactConsent = "contact-v1"

type ActivationScope struct {
	Installation          uuid.UUID `json:"installation_id"`
	Integration           uuid.UUID `json:"integration_id"`
	Account               int64     `json:"amo_account_id"`
	Generation            int64     `json:"generation"`
	AuthorizationSequence int64     `json:"authorization_sequence"`
}

type ActivationContact struct {
	UserID  int64  `json:"amo_user_id"`
	Name    string `json:"name"`
	Email   string `json:"email,omitempty"`
	Role    string `json:"role,omitempty"`
	Phone   string `json:"phone"`
	Consent string `json:"consent_version"`
}

type ActivationRequest struct {
	Scope   ActivationScope    `json:"scope"`
	Contact *ActivationContact `json:"contact,omitempty"`
}

type ActivationResult struct {
	Activated bool `json:"activated"`
}

func ContactPhone(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	var digits strings.Builder
	if len(value) > 48 {
		return "", errors.New("invalid phone")
	}
	for i, ch := range value {
		switch {
		case ch >= '0' && ch <= '9':
			digits.WriteRune(ch)
		case ch == '+' && i == 0, unicode.IsSpace(ch), ch == '(', ch == ')', ch == '-', ch == '.':
		default:
			return "", errors.New("invalid phone")
		}
	}
	phone := digits.String()
	if len(phone) == 11 && phone[0] == '8' && !strings.HasPrefix(value, "+") {
		phone = "7" + phone[1:]
	}
	if len(phone) < 8 || len(phone) > 15 || phone[0] == '0' {
		return "", errors.New("invalid phone")
	}
	return "+" + phone, nil
}
