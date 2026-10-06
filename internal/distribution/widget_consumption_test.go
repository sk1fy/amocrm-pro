package distribution

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWidgetRequestConsumesToken(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   bool
	}{
		{"bootstrap is a read", "GET", "/api/v1/widget/distribution/bootstrap", "", false},
		{"permissions is a read", "POST", "/api/v1/widget/distribution/permissions", `{"leadId":"1"}`, false},
		{"runtime without write is a read", "POST", "/api/v1/widget/distribution/runtime", `{"kind":"groups"}`, false},
		{"runtime write=false is a read", "POST", "/api/v1/widget/distribution/runtime", `{"kind":"groups","write":false}`, false},
		{"runtime write=true consumes", "POST", "/api/v1/widget/distribution/runtime", `{"kind":"rule","write":true}`, true},
		{"runtime unknown kind with write=true consumes", "POST", "/api/v1/widget/distribution/runtime", `{"kind":"nonsense","write":true}`, true},
		{"runtime write=true malformed fails closed", "POST", "/api/v1/widget/distribution/runtime", `{"write":tr`, true},
		{"runtime unparseable runtime fails closed", "POST", "/api/v1/widget/distribution/runtime", `not-json`, true},
		{"runtime oversized fails closed", "POST", "/api/v1/widget/distribution/runtime", strings.Repeat("a", MaxBody+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if got := widgetRequestConsumesToken(r); got != tc.want {
				t.Fatalf("consume=%v want %v", got, tc.want)
			}
			// The probe must not consume the body: the handler still needs it.
			buf := make([]byte, len(tc.body))
			n, _ := r.Body.Read(buf)
			if string(buf[:n]) != tc.body {
				t.Fatalf("body mutated by probe: %q", string(buf[:n]))
			}
		})
	}
}
