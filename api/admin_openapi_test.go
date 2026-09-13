package api

import (
	"context"
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
