package adminread

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
)

const (
	codeUnauthenticated = "unauthenticated"
	codeInvalidArgument = "invalid_argument"
	codeNotFound        = "not_found"
	codeInternal        = "internal"
)

type apiError struct {
	status  int
	code    string
	message string
}

func (e apiError) Error() string { return e.message }

func errUnauthenticated() error {
	return apiError{status: http.StatusUnauthorized, code: codeUnauthenticated, message: "authentication required"}
}

func errInvalid(message string) error {
	return apiError{status: http.StatusBadRequest, code: codeInvalidArgument, message: message}
}

func errNotFound(message string) error {
	return apiError{status: http.StatusNotFound, code: codeNotFound, message: message}
}

type jsonError struct {
	Error jsonErrorFields `json:"error"`
}

type jsonErrorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var api apiError
	if !errors.As(err, &api) {
		api = apiError{status: http.StatusInternalServerError, code: codeInternal, message: "internal error"}
	}
	if api.code == codeUnauthenticated {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	requestID := w.Header().Get("X-Request-ID")
	if requestID == "" && r != nil {
		requestID = httpmiddleware.RequestIDFromContext(r.Context()).String()
	}
	writeJSON(w, api.status, jsonError{Error: jsonErrorFields{
		Code: api.code, Message: api.message, RequestID: requestID,
	}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
