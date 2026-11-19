package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompanyCabinetActivation(t *testing.T) {
	const origin = "https://" + companyCabinetHost
	for _, tc := range []struct {
		name, origin, query, body string
		active, unavailable       bool
		status                    int
	}{
		{name: "unknown origin", origin: "https://fixture.example.invalid", status: 403},
		{name: "query rejected", origin: origin, query: "?account=other", status: 400},
		{name: "inactive", origin: origin, status: 200, body: "{\"activated\":false}\n"},
		{name: "active", origin: origin, active: true, status: 200, body: "{\"activated\":true}\n"},
		{name: "unavailable", origin: origin, unavailable: true, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			handler := companyCabinetActivation(func(context.Context) (bool, error) {
				called = true
				if tc.unavailable {
					return false, errors.New("fixture unavailable")
				}
				return tc.active, nil
			})
			request := httptest.NewRequest(http.MethodGet, "/api/v1/company-cabinet/activation"+tc.query, nil)
			request.Header.Set("Origin", tc.origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || response.Body.String() != tc.body || called != (tc.status == http.StatusOK || tc.unavailable) {
				t.Fatalf("status=%d body=%q checked=%t", response.Code, response.Body.String(), called)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); (got == origin) != (tc.origin == origin) {
				t.Fatalf("unexpected CORS response %q", got)
			}
		})
	}
}
