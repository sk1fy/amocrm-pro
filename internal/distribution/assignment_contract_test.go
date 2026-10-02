package distribution

import (
	"encoding/json"
	"github.com/oasdiff/yaml3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func runtimeContract(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	paths := []string{"../../api/distribution-openapi.yaml", "../../docs/specs/lead-distribution-v1/contracts/teamos-distribution.openapi.yaml", "../../docs/specs/lead-distribution-v1/contracts/core-distribution.openapi.yaml"}
	base := ""
	for _, p := range paths {
		abs, e := filepath.Abs(p)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := os.ReadFile(abs)
		if e != nil {
			t.Fatal(e)
		}
		var v map[string]any
		if e = yaml.Unmarshal(raw, &v); e != nil {
			t.Fatal(e)
		}
		j, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		var value any
		if e = json.Unmarshal(j, &value); e != nil {
			t.Fatal(e)
		}
		uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
		if base == "" {
			base = uri
		}
		if e = compiler.AddResource(uri, value); e != nil {
			t.Fatal(e)
		}
	}
	schema, e := compiler.Compile(base + "#/components/schemas/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return schema
}
func validateRuntimeResponse(t *testing.T, name string, body []byte) {
	t.Helper()
	var v any
	if e := json.Unmarshal(body, &v); e != nil {
		t.Fatal(e)
	}
	if e := runtimeContract(t, name).Validate(v); e != nil {
		t.Fatalf("%s response failed schema: %v body %s", name, e, body)
	}
}
func TestRuntimeOperationContractSchemasCompile(t *testing.T) {
	for _, name := range []string{"AssignmentEnvelope", "OperationReceipt", "Operation", "ResultEnvelope", "ReconcileInput"} {
		runtimeContract(t, name)
	}
}
