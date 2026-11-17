package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestPlatformV1OpenAPIContract(t *testing.T) {
	document := loadPlatformV1(t)
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			switch path {
			case "/api/v1/widget/platform-access-proof":
				if operation.Security == nil || len(*operation.Security) != 2 {
					t.Fatal("Core proof issuance requires one of the existing widget credentials")
				}
				for i, scheme := range []string{"coreWidgetSDK", "coreWidgetBearer"} {
					if alternative := (*operation.Security)[i]; len(alternative) != 1 || alternative[scheme] == nil {
						t.Errorf("unexpected Core widget auth alternative %d: %v", i, alternative)
					}
				}
			case "/v1/access/{amo_account_id}/{product_code}":
				assertPlatformSecurity(t, operation, "widgetProof")
			case "/v1/installations/{id}/activation":
				assertPlatformSecurity(t, operation, "activationProof")
			case "/internal/platform/v1/core-events":
				assertPlatformSecurity(t, operation, "coreService")
			default:
				assertPlatformSecurity(t, operation, "adminService")
				if operation.Extensions["x-platform-grant"] == nil ||
					operation.Parameters.GetByInAndName("header", "X-Platform-Actor") == nil {
					t.Errorf("%s %s must declare the actor envelope and a permission", method, path)
				}
			}
		}
	}
	installations := document.Paths.Find("/internal/platform/v1/managers/installations").Get
	limit := installations.Parameters.GetByInAndName("query", "limit").Schema.Value
	defaultLimit, _ := json.Marshal(limit.Default)
	if string(defaultLimit) != "25" || limit.Max == nil || *limit.Max != 100 {
		t.Errorf("list limit = %v, want default 25 and maximum 100", limit)
	}
	review := document.Paths.Find("/internal/platform/v1/managers/installations/{id}/review").Put
	for _, key := range []string{"If-Match", "Idempotency-Key"} {
		if param := review.Parameters.GetByInAndName("header", key); param == nil || !param.Required {
			t.Errorf("review is missing required %s", key)
		}
	}
}

func TestPlatformV1LifecycleExamples(t *testing.T) {
	document := loadPlatformV1(t)
	operation := document.Paths.Find("/internal/platform/v1/core-events").Post
	schema := operation.RequestBody.Value.Content["application/json"].Schema.Value
	for _, tc := range []struct {
		kind       string
		generation float64
		sequence   float64
		source     string
	}{
		{"installed", 1, 1, "core_oauth"},
		{"reauthorized", 1, 2, "core_oauth"},
		{"uninstalled", 1, 3, "core_uninstall"},
		{"reinstalled", 2, 4, "core_oauth"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "platform-v1", tc.kind+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if err := schema.VisitJSON(event, openapi3.EnableFormatValidation()); err != nil {
				t.Fatal(err)
			}
			evidence, ok := event["evidence"].(map[string]any)
			if !ok || event["kind"] != tc.kind || event["generation"] != tc.generation ||
				event["sequence"] != tc.sequence || evidence["source"] != tc.source || event["amo_account_id"] != "9007199254740993" {
				t.Fatalf("example breaks lifecycle sequence or loses exact amoCRM ID")
			}
			event["amo_account_id"] = float64(9007199254740993)
			if err := schema.VisitJSON(event); err == nil {
				t.Fatal("accepted numeric amoCRM account ID")
			}
			event["amo_account_id"] = "9223372036854775808"
			if err := schema.VisitJSON(event); err == nil {
				t.Fatal("accepted amoCRM account ID above int64")
			}
			event["amo_account_id"] = "9223372036854775807"
			if err := schema.VisitJSON(event); err != nil {
				t.Fatalf("rejected largest valid amoCRM account ID: %v", err)
			}
			event["provider_payload"] = map[string]any{}
			if err := schema.VisitJSON(event); err == nil {
				t.Fatal("accepted raw provider payload")
			}
		})
	}
}

func loadPlatformV1(t *testing.T) *openapi3.T {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromFile("platform-v1-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return document
}

func assertPlatformSecurity(t *testing.T, operation *openapi3.Operation, scheme string) {
	t.Helper()
	if operation.Security == nil || len(*operation.Security) != 1 || len((*operation.Security)[0]) != 1 {
		t.Fatalf("%s must use only %s authentication", operation.OperationID, scheme)
	}
	if _, ok := (*operation.Security)[0][scheme]; !ok {
		t.Errorf("%s must use %s authentication", operation.OperationID, scheme)
	}
}
