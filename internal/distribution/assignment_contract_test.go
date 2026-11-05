package distribution

import (
	"encoding/json"
	"github.com/oasdiff/yaml3"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runtimeContract(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	paths := []string{"../../api/distribution-openapi.yaml"}
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

func TestManualOperationActionRequiresMappedIdentity(t *testing.T) {
	const teamOSID = `"teamosUserId":"8b85b923-1305-44f6-a1f8-917749cffba6"`
	const crmID = `"crmUserId":"42"`
	for _, c := range []struct {
		name, fields string
		allowed      bool
	}{
		{"complete", "," + teamOSID + "," + crmID, true},
		{"missing Team OS ID", "," + crmID, false},
		{"missing CRM ID", "," + teamOSID, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := `{"expectedResultVersion":1,"reason":"review","actor":{"kind":"manual"` + c.fields + `}}`
			_, err := readAction(httptest.NewRequest("POST", "/", strings.NewReader(body)))
			if (err == nil) != c.allowed {
				t.Fatalf("readAction error = %v; want allowed = %v", err, c.allowed)
			}
		})
	}
}
