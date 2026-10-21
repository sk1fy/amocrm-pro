package amocrm

import (
	"context"
	"errors"
)

// FailureDiagnostic exposes bounded facts without error strings, URLs,
// credentials, response bodies or SQL arguments.
func FailureDiagnostic(err error) (reason string, upstreamStatus int) {
	var apiError *APIError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded", 0
	case errors.Is(err, context.Canceled):
		return "request_canceled", 0
	case errors.Is(err, ErrOverloaded):
		return "crm_budget_overloaded", 0
	case errors.As(err, &apiError):
		return "crm_http_error", apiError.StatusCode
	case errors.Is(err, ErrTransport):
		return "crm_transport_error", 0
	case errors.Is(err, ErrIncompleteResponse):
		return "crm_response_incomplete", 0
	default:
		return "internal_error", 0
	}
}
