package amocrm

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"testing"
	"time"
)

func TestDistributionSnapshotAbsenceAndDeleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{{204, "", ErrLeadAbsent}, {200, `{"id":11,"is_deleted":true}`, ErrLeadDeleted}, {200, `{"id":12,"is_deleted":true}`, ErrIncompleteResponse}, {200, `{}`, ErrIncompleteResponse}, {200, "", ErrIncompleteResponse}, {403, "", nil}, {404, "", nil}} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, e := c.GetLeadSnapshot(context.Background(), uuid.New(), 11)
		if tc.want != nil && !errors.Is(e, tc.want) {
			t.Fatal(tc, e)
		}
		if tc.want == nil && (e == nil || errors.Is(e, ErrLeadAbsent) || errors.Is(e, ErrLeadDeleted)) {
			t.Fatal("undocumented status converted to absence", tc, e)
		}
	}
}
func TestDistributionBoundedScanQueriesAndCompleteness(t *testing.T) {
	from := time.Unix(100, 0)
	to := time.Unix(200, 0)
	for _, tc := range []struct {
		status int
		body   string
		valid  bool
	}{{204, "", true}, {200, `{"_embedded":{"leads":[]},"_links":{}}`, true}, {200, `{"_embedded":{"leads":[{"id":11}]},"_links":{"next":{"href":"/api/v4/leads?page=2"}}}`, true}, {200, `{}`, false}, {200, `{"_embedded":{"leads":null},"_links":{}}`, false}, {200, `{"_embedded":{"leads":[{"id":11},{"id":11}]},"_links":{}}`, false}} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("limit") != "250" || q.Get("filter[updated_at][from]") != "100" || q.Get("filter[updated_at][to]") != "200" || q.Get("page") != "1" {
				t.Error("unbounded query", q)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, e := c.DistributionLeadPage(context.Background(), uuid.New(), from, to, 1)
		if (e == nil) != tc.valid {
			t.Fatal(tc, e)
		}
	}
}
