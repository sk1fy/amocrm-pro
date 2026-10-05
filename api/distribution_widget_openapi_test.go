package api

import (
	"context"
	"github.com/getkin/kin-openapi/openapi3"
	"testing"
)

func TestDistributionWidgetContract(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, e := loader.LoadFromFile("distribution-widget-openapi.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
	if doc.Paths.Find("/api/v1/widget/distribution/runtime").Post == nil || doc.Paths.Find("/api/v1/widget/distribution/bootstrap").Get == nil {
		t.Fatal("missing widget operations")
	}
}
