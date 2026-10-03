package distribution

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestLiveLeadInterfacePresentationUsesValidatedInstallationDomain(t *testing.T) {
	f := assignmentFixture(t)
	f.CRM.Lead.Name = "Fixture deal"
	h := assignmentHTTP(t, f)
	path := "/internal/v1/distribution/bindings/" + f.Assignment.Scope.BindingID.String() + "/leads/10"
	read := func(want int) LeadObservation {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		Sign(r, f.Scope, h.secret, nil)
		out := httptest.NewRecorder()
		h.router.ServeHTTP(out, r)
		if out.Code != want {
			t.Fatalf("%d %s", out.Code, out.Body)
		}
		var lead LeadObservation
		if want == 200 {
			validateRuntimeResponse(t, "LeadObservation", out.Body.Bytes())
			if json.Unmarshal(out.Body.Bytes(), &lead) != nil {
				t.Fatal(out.Body)
			}
		}
		return lead
	}
	o := read(200)
	if o.LeadURL == nil || *o.LeadURL != "https://test.amocrm.ru/leads/detail/10" || o.LeadName == nil || *o.LeadName != "Fixture deal" {
		t.Fatalf("presentation %+v", o)
	}
	for _, domain := range []string{"https://evil.invalid", "https://test.amocrm.ru@evil.invalid", "test.amocrm.ru/leads/evil", "https://test.amocrm.ru:444", "test.amocrm.ru.evil.invalid"} {
		if _, e := f.Pool.Exec(context.Background(), "UPDATE installations SET account_domain=$2 WHERE id=$1", f.Scope.InstallationID, domain); e != nil {
			t.Fatal(e)
		}
		if read(200).LeadURL != nil {
			t.Fatal("untrusted URL", domain)
		}
	}
	if _, e := f.Pool.Exec(context.Background(), "UPDATE distribution_bindings SET state='revoked' WHERE id=$1", f.Assignment.Scope.BindingID); e != nil {
		t.Fatal(e)
	}
	read(403)
}
