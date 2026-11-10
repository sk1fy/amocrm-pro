package api

import (
	"context"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestCompanyEntryOpenAPI(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("company-entry-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/widget/company/status", "/api/v1/widget/company/entries", "/internal/v1/company-entry/subject-check"} {
		item := doc.Paths.Find(path)
		if item == nil || item.Post == nil || item.Post.Security == nil {
			t.Fatalf("missing secured POST %s", path)
		}
	}
}
