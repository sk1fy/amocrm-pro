package api

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
)

func TestOpenAPIContract(t *testing.T) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	document, err := loader.LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatalf("load OpenAPI contract: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI contract: %v", err)
	}

	expected := make(map[string][]string, len(apicontract.Routes))
	for _, route := range apicontract.Routes {
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
			operation := item.GetOperation(method)
			if operation == nil {
				t.Errorf("OpenAPI operation %s %s is missing", method, path)
				continue
			}
			if len(path) >= len("/api/v1/widget/") && path[:len("/api/v1/widget/")] == "/api/v1/widget/" {
				assertWidgetSecurityAlternatives(t, method, path, operation.Security)
			}
		}
	}

	widgetToken := document.Components.SecuritySchemes["widgetToken"]
	if widgetToken == nil || widgetToken.Value == nil ||
		widgetToken.Value.Type != "apiKey" || widgetToken.Value.In != "header" ||
		widgetToken.Value.Name != "X-Auth-Token" {
		t.Fatalf("widgetToken security scheme = %+v, want X-Auth-Token header apiKey", widgetToken)
	}
	widgetBearer := document.Components.SecuritySchemes["widgetBearer"]
	if widgetBearer == nil || widgetBearer.Value == nil ||
		widgetBearer.Value.Type != "http" || widgetBearer.Value.Scheme != "bearer" {
		t.Fatalf("widgetBearer security scheme = %+v, want HTTP bearer", widgetBearer)
	}
}

func assertWidgetSecurityAlternatives(
	t *testing.T,
	method string,
	path string,
	requirements *openapi3.SecurityRequirements,
) {
	t.Helper()
	if requirements == nil || len(*requirements) != 2 {
		t.Errorf("OpenAPI operation %s %s security = %+v, want two alternatives", method, path, requirements)
		return
	}
	wants := map[string]bool{"widgetToken": false, "widgetBearer": false}
	for _, requirement := range *requirements {
		if len(requirement) != 1 {
			t.Errorf("OpenAPI operation %s %s security requirement = %+v, want one scheme per alternative", method, path, requirement)
			continue
		}
		for name := range requirement {
			if _, ok := wants[name]; !ok {
				t.Errorf("OpenAPI operation %s %s has unexpected security scheme %q", method, path, name)
				continue
			}
			wants[name] = true
		}
	}
	for name, found := range wants {
		if !found {
			t.Errorf("OpenAPI operation %s %s is missing %s alternative", method, path, name)
		}
	}
}
