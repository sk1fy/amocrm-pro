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
			if item.GetOperation(method) == nil {
				t.Errorf("OpenAPI operation %s %s is missing", method, path)
			}
		}
	}
}
