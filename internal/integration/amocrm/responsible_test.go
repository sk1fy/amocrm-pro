package amocrm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func responsibleTestClient(t *testing.T, handler http.Handler) (*Client, *fakeTokenProvider) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider := &fakeTokenProvider{baseURL: server.URL}
	c := NewClient(server.Client(), provider)
	c.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	return c, provider
}
func TestGetLeadSnapshotPresenceAndLegacyCompatibility(t *testing.T) {
	valid := `{"id":11,"name":"Сделка","pipeline_id":1,"status_id":2,"responsible_user_id":9007199254740993,"updated_at":1790935200}`
	for _, body := range []string{valid, `{"id":11,"pipeline_id":1,"status_id":2}`, strings.Replace(valid, `"responsible_user_id":9007199254740993,`, "", 1), strings.Replace(valid, `"updated_at":1790935200`, `"updated_at":null`, 1), strings.Replace(valid, `"status_id":2`, `"status_id":0`, 1), strings.Replace(valid, `"name":"Сделка"`, `"name":null`, 1), strings.Replace(valid, `"id":11`, `"id":12`, 1)} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		out, err := c.GetLeadSnapshot(context.Background(), uuid.New(), 11)
		if body == valid {
			if err != nil || out.ResponsibleUserID != 9007199254740993 || out.Name != "Сделка" || out.UpdatedAt != 1790935200 {
				t.Fatalf("snapshot %+v %v", out, err)
			}
		} else if !errors.Is(err, ErrIncompleteResponse) {
			t.Fatalf("accepted incomplete %s: %v", body, err)
		}
		if body == `{"id":11,"pipeline_id":1,"status_id":2}` {
			if _, err = c.GetLeadState(context.Background(), uuid.New(), 11); err != nil {
				t.Fatalf("legacy contract broken %v", err)
			}
		}
	}
}
func TestPreparedResponsibleSendsOneNarrowPatchAndDoesNotRefresh401(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			c, provider := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "PATCH" || r.URL.Path != "/api/v4/leads/11" || r.Header.Get("Authorization") != "Bearer old" {
					t.Error("wrong mutation scope")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body) != 1 || string(body["responsible_user_id"]) != "9007199254740993" {
					t.Errorf("unexpected mutation fields %v", body)
				}
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = w.Write([]byte(`{"_embedded":{"leads":[{"id":11,"updated_at":1790935200}]}}`))
				}
			}))
			mutation, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 0 {
				t.Fatal("preparation sent PATCH")
			}
			result, err := mutation.Assign(context.Background(), 11, 9007199254740993)
			if status == 200 {
				if err != nil || !result.Accepted || result.UpdatedAt != 1790935200 {
					t.Fatalf("ack %+v %v", result, err)
				}
			} else {
				var dispatched *ResponsibleDispatchError
				var api *APIError
				if !errors.As(err, &dispatched) || !dispatched.Dispatched || dispatched.StatusCode != status || !errors.As(err, &api) {
					t.Fatalf("classification %v", err)
				}
				if status == 429 && api.RetryAfter != 9*time.Second {
					t.Fatalf("retry-after %+v", api)
				}
			}
			_, err = mutation.Assign(context.Background(), 11, 7)
			var local *ResponsibleDispatchError
			if !errors.As(err, &local) || local.Dispatched || requests.Load() != 1 {
				t.Fatalf("prepared replay %v requests%d", err, requests.Load())
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if status == 401 && provider.markedReauth != 1 {
				t.Fatal("401 did not mark reauthorization")
			}
			if status != 401 && provider.markedReauth != 0 {
				t.Fatal("non401 marked reauthorization")
			}
			if len(provider.requests) != 1 || provider.requests[0] {
				t.Fatal("PATCH silently refreshed/retried OAuth")
			}
		})
	}
}
func TestResponsibleIncompleteAcknowledgementRemainsDispatched(t *testing.T) {
	for _, body := range []string{"", `{`, `{}`, `{"id":12,"updated_at":1}`, `{"id":11}`, `{"id":11,"updated_at":null}`, `{"id":11,"updated_at":0}`, `{"id":11,"updated_at":1,"_embedded":{"leads":[{"id":12,"updated_at":1}]}}`, `{"_embedded":{"leads":[{"id":12,"updated_at":1}]}}`, `{"_embedded":{"leads":[{"id":11}]}}`, `{"_embedded":{"leads":[{"id":11,"updated_at":0}]}}`, strings.Repeat("x", maxAPIResponseBody+1)} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		m, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		result, err := m.Assign(context.Background(), 11, 7)
		var dispatched *ResponsibleDispatchError
		if result.Accepted || !errors.As(err, &dispatched) || !dispatched.Dispatched {
			t.Fatalf("false confirmation %v %+v", err, result)
		}
	}
}

func TestResponsibleAcceptsSingleAndCollectionAcknowledgements(t *testing.T) {
	for _, body := range []string{
		`{"id":11,"updated_at":1790935200,"_links":{"self":{"href":"https://fixture.amocrm.test/api/v4/leads/11"}}}`,
		`{"_embedded":{"leads":[{"id":11,"updated_at":1790935200}]}}`,
	} {
		var requests atomic.Int32
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			_, _ = w.Write([]byte(body))
		}))
		m, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		out, err := m.Assign(context.Background(), 11, 7)
		if err != nil || !out.Accepted || out.UpdatedAt != 1790935200 || requests.Load() != 1 {
			t.Fatalf("valid acknowledgement rejected: %+v %v calls %d", out, err, requests.Load())
		}
	}
}

type responsibleRoundTrip func(*http.Request) (*http.Response, error)

func (f responsibleRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestResponsibleTimeoutAndLocalCancellationDiffer(t *testing.T) {
	c, _ := responsibleTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected real transport") }))
	var requests atomic.Int32
	c.httpClient = &http.Client{Transport: responsibleRoundTrip(func(*http.Request) (*http.Response, error) { requests.Add(1); return nil, context.DeadlineExceeded })}
	m, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = m.Assign(ctx, 11, 7)
	var dispatched *ResponsibleDispatchError
	if !errors.As(err, &dispatched) || dispatched.Dispatched || requests.Load() != 0 {
		t.Fatalf("local cancellation %v", err)
	}
	_, err = m.Assign(context.Background(), 11, 7)
	if !errors.As(err, &dispatched) || !dispatched.Dispatched || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrTransport) || requests.Load() != 1 {
		t.Fatalf("uncertain timeout %v", err)
	}
}
func TestResponsibleBodyReadFailureIsUncertain(t *testing.T) {
	c, _ := responsibleTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected transport") }))
	c.httpClient = &http.Client{Transport: responsibleRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(errorReader{})}, nil
	})}
	m, e := c.PrepareLeadResponsible(context.Background(), uuid.New())
	if e != nil {
		t.Fatal(e)
	}
	_, e = m.Assign(context.Background(), 11, 7)
	var dispatched *ResponsibleDispatchError
	if !errors.As(e, &dispatched) || !dispatched.Dispatched || !errors.Is(e, ErrTransport) {
		t.Fatalf("read failure %v", e)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestResponsibleRedirectNeverReplaysPatch(t *testing.T) {
	var patches atomic.Int32
	c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patches.Add(1)
		w.Header().Set("Location", "/api/v4/leads/11?again=1")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	m, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Assign(context.Background(), 11, 7)
	var dispatched *ResponsibleDispatchError
	if !errors.As(err, &dispatched) || !dispatched.Dispatched || dispatched.StatusCode != 307 || patches.Load() != 1 {
		t.Fatalf("redirect replay %v calls%d", err, patches.Load())
	}
}

func TestLeadSnapshotSourceFailuresStayClassified(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   ErrorKind
	}{{403, ErrorForbidden}, {404, ErrorNotFound}, {429, ErrorRateLimited}, {503, ErrorTemporary}} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(tc.status)
		}))
		_, err := c.GetLeadSnapshot(context.Background(), uuid.New(), 11)
		var api *APIError
		if !errors.As(err, &api) || api.Kind != tc.kind || api.StatusCode != tc.status {
			t.Fatalf("source classification %v", err)
		}
	}
	c, _ := responsibleTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected transport") }))
	c.httpClient = &http.Client{Transport: responsibleRoundTrip(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}
	_, err := c.GetLeadSnapshot(context.Background(), uuid.New(), 11)
	if !errors.Is(err, ErrTransport) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("source timeout %v", err)
	}
}
func TestResponsibleUnexpectedSuccessCodesNeedReconciliation(t *testing.T) {
	for _, status := range []int{201, 204} {
		c, _ := responsibleTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		m, err := c.PrepareLeadResponsible(context.Background(), uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		out, err := m.Assign(context.Background(), 11, 7)
		var dispatched *ResponsibleDispatchError
		if out.Accepted || !errors.As(err, &dispatched) || !dispatched.Dispatched || dispatched.StatusCode != status || !errors.Is(err, ErrIncompleteResponse) {
			t.Fatalf("unexpected success %d %+v %v", status, out, err)
		}
	}
}
