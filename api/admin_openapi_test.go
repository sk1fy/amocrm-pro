package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
)

func TestAdminOpenAPIContract(t *testing.T) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	document, err := loader.LoadFromFile("admin-openapi.yaml")
	if err != nil {
		t.Fatalf("load admin OpenAPI contract: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate admin OpenAPI contract: %v", err)
	}

	expected := make(map[string][]string, len(apicontract.AdminRoutes))
	for _, route := range apicontract.AdminRoutes {
		expected[route.Path] = append(expected[route.Path], route.Method)
	}
	for _, route := range apicontract.AdminCommandRoutes {
		expected[route.Path] = append(expected[route.Path], route.Method)
	}
	for _, route := range apicontract.AdminCRMRoutes {
		expected[route.Path] = append(expected[route.Path], route.Method)
	}
	if document.Paths.Len() != len(expected) {
		t.Fatalf("OpenAPI paths = %d, expected %d", document.Paths.Len(), len(expected))
	}
	for path, methods := range expected {
		item := document.Paths.Find(path)
		if item == nil {
			t.Errorf("OpenAPI path %s is missing", path)
			continue
		}
		operations := item.Operations()
		if len(operations) != len(methods) {
			t.Errorf("OpenAPI path %s methods = %v, expected %v", path, operations, methods)
			continue
		}
		for _, method := range methods {
			if item.GetOperation(method) == nil {
				t.Errorf("OpenAPI operation %s %s is missing", method, path)
			}
		}
	}

	adminBearer := document.Components.SecuritySchemes["adminBearer"]
	if adminBearer == nil || adminBearer.Value == nil ||
		adminBearer.Value.Type != "http" || adminBearer.Value.Scheme != "bearer" {
		t.Fatalf("adminBearer security scheme = %+v, want HTTP bearer", adminBearer)
	}
	if _, ok := document.Components.SecuritySchemes["widgetToken"]; ok {
		t.Fatal("admin OpenAPI must not define widgetToken")
	}
	if _, ok := document.Components.SecuritySchemes["widgetBearer"]; ok {
		t.Fatal("admin OpenAPI must not define widgetBearer")
	}
}

func TestTaskControlDiagnosticsExampleRejectsUnknownData(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("admin-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../docs/examples/task-control-diagnostics.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	schema := document.Components.Schemas["TaskControlDiagnostics"].Value
	if err = schema.VisitJSON(fixture); err != nil {
		t.Fatal(err)
	}
	fixture["unexpected_provider_payload"] = map[string]any{"token": "secret", "rules": []any{}}
	if err = schema.VisitJSON(fixture); err == nil {
		t.Fatal("diagnostic contract accepted unknown subtree")
	}
}

func TestCRMUsersResponseRejectsContactFields(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("admin-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := document.Components.Schemas["CRMUsersResponse"].Value
	valid := func() map[string]any {
		return map[string]any{
			"source": "core", "observed_at": "2026-11-06T10:00:00Z", "account_id": 31415926,
			"state": "ok", "retry_after_seconds": nil, "fresh_until": "2026-11-06T10:05:00Z", "total": 1,
			"connection": map[string]any{"installation_id": "11111111-1111-4111-8111-111111111111", "integration_id": "22222222-2222-4222-8222-222222222222", "integration_code": "widget-fixture-a"},
			"items":      []any{map[string]any{"id": 10, "name": "Первый", "is_active": true, "is_admin": false}},
		}
	}
	if err = schema.VisitJSON(valid()); err != nil {
		t.Fatal(err)
	}
	unknown := valid()
	unknown["state"], unknown["items"], unknown["total"], unknown["connection"], unknown["fresh_until"] = "no_authorized_connection", nil, nil, nil, nil
	if err = schema.VisitJSON(unknown); err != nil {
		t.Fatalf("unknown list must be representable as null: %v", err)
	}
	withEmail := valid()
	withEmail["items"] = []any{map[string]any{"id": 10, "name": "Первый", "is_active": true, "is_admin": false, "email": "user@example.invalid"}}
	if err = schema.VisitJSON(withEmail); err == nil {
		t.Fatal("CRM users contract accepted a contact field")
	}
	withRights := valid()
	withRights["rights"] = map[string]any{"is_admin": true}
	if err = schema.VisitJSON(withRights); err == nil {
		t.Fatal("CRM users contract accepted rights")
	}
}
