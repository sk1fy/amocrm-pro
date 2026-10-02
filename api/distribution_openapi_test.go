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
	if doc.Paths.Len() != 4 {
		t.Fatal("unexpectedprivatepaths", doc.Paths.Len())
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security == nil || len(*op.Security) != 1 {
				t.Fatal("missingprivateauth", method, path)
			}
			if method == "POST" {
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
