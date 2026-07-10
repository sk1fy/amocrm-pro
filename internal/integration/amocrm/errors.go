package amocrm

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type ErrorKind string

const (
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorPayment      ErrorKind = "payment_required"
	ErrorForbidden    ErrorKind = "forbidden"
	ErrorNotFound     ErrorKind = "not_found"
	ErrorValidation   ErrorKind = "validation"
	ErrorRateLimited  ErrorKind = "rate_limited"
	ErrorTemporary    ErrorKind = "temporary"
	ErrorPermanent    ErrorKind = "permanent"
)

type APIError struct {
	Kind       ErrorKind
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("amoCRM API returned HTTP %d (%s)", e.StatusCode, e.Kind)
}

func classifyResponse(status int, header http.Header, now time.Time) *APIError {
	errorResponse := &APIError{StatusCode: status, Kind: ErrorPermanent}
	switch {
	case status == http.StatusUnauthorized:
		errorResponse.Kind = ErrorUnauthorized
	case status == http.StatusPaymentRequired:
		errorResponse.Kind = ErrorPayment
	case status == http.StatusForbidden:
		errorResponse.Kind = ErrorForbidden
	case status == http.StatusNotFound:
		errorResponse.Kind = ErrorNotFound
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		errorResponse.Kind = ErrorValidation
	case status == http.StatusTooManyRequests:
		errorResponse.Kind = ErrorRateLimited
		errorResponse.Retryable = true
		errorResponse.RetryAfter = retryAfter(header.Get("Retry-After"), now)
	case status == http.StatusRequestTimeout || status == http.StatusConflict || status >= 500:
		errorResponse.Kind = ErrorTemporary
		errorResponse.Retryable = true
	}
	return errorResponse
}

func retryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(raw); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 5 * time.Second
}
