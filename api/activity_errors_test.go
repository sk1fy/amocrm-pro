package api

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
)

func TestActivityRateLimitResponseVariants(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range activitybridge.Routes() {
		schema := document.Paths.Find(route.Path).GetOperation(route.Method).Responses.Status(429).Value.Content["application/json"].Schema.Value
		for _, tc := range []struct {
			name  string
			body  map[string]any
			valid bool
		}{
			{"widget admission", map[string]any{"code": "rate_limited"}, true},
			{"downstream capacity", map[string]any{"code": "resource_exhausted", "message": "capacity exhausted", "request_id": "request-1", "retryable": true}, true},
			{"incomplete downstream error", map[string]any{"code": "resource_exhausted"}, false},
			{"unknown code", map[string]any{"code": "unknown"}, false},
		} {
			t.Run(route.Method+" "+route.Path+"/"+tc.name, func(t *testing.T) {
				err := schema.VisitJSON(map[string]any{"error": tc.body})
				if (err == nil) != tc.valid {
					t.Fatalf("valid=%v error=%v", tc.valid, err)
				}
			})
		}
	}
}
