package api

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestCompanyCRMOpenAPIContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("company-crm-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Extensions["x-mutual-tls"]; !ok {
		t.Fatal("missing required mTLS transport contract")
	}
	if doc.Paths.Len() != 6 {
		t.Fatal(doc.Paths.Len())
	}
	for path, method := range map[string]string{"/v1/company-crm/installation-deals/title": "POST", "/v1/company-crm/staff": "GET", "/v1/company-crm/clients": "GET", "/v1/company-crm/workflow-metadata": "GET", "/v1/company-crm/validate-workflow": "POST", "/v1/company-crm/installation-deals": "POST"} {
		if doc.Paths.Find(path) == nil || doc.Paths.Find(path).GetOperation(method) == nil {
			t.Fatal(path, method)
		}
	}
}
