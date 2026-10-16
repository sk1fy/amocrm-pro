package api

import (
	"context"
	"encoding/json"
	"github.com/getkin/kin-openapi/openapi3"
	"os"
	"testing"
)

func TestTaskControlReferenceContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("task-control-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	op := doc.Paths.Find("/internal/task-control/accounts/{accountId}/references").Get
	if op == nil || op.RequestBody != nil || op.Security == nil || len(*op.Security) != 1 {
		t.Fatal("incomplete reference transport contract")
	}
	for _, p := range op.Parameters {
		if p.Value.In == "query" {
			t.Fatal("query must not be accepted")
		}
	}
	raw, err := os.ReadFile("../internal/crmreferences/testdata/reference-snapshot-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture any
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["ReferenceSnapshotV1"].Value.VisitJSON(fixture); err != nil {
		t.Fatal("shared fixture violates Core wire contract", err)
	}

}
