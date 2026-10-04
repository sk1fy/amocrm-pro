package api

import (
	"context"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestPrivateDistributionOperationContract(t *testing.T) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	doc, e := loader.LoadFromFile("distribution-openapi.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if doc.Paths.Len() != 10 {
		t.Fatal("unexpectedprivatepaths", doc.Paths.Len())
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security == nil || len(*op.Security) != 1 {
				t.Fatal("missingprivateauth", method, path)
			}
			if method == "POST" && (path == "/internal/v1/distribution/assignments" || path == "/internal/v1/distribution/assignments/expire" || path == "/internal/v1/distribution/operations/{operationId}/cancel" || path == "/internal/v1/distribution/operations/{operationId}/reconcile") {
				found := false
				for _, p := range op.Parameters {
					if p.Value != nil && p.Value.Name == "Idempotency-Key" && p.Value.Required {
						found = true
					}
				}
				if !found {
					t.Fatal("missingidempotency", path)
				}
			}
		}
	}
}
