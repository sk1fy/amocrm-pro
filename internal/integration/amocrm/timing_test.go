package amocrm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/requesttiming"
)

type timingTransport func(*http.Request) (*http.Response, error)

func (f timingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type timingBody struct{ closed bool }

func (*timingBody) Read([]byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	return 0, errors.New("customer secret read failure")
}
func (b *timingBody) Close() error { time.Sleep(10 * time.Millisecond); b.closed = true; return nil }
func TestTimingIncludesBodyFailureAndCloseWithoutLeaks(t *testing.T) {
	body := &timingBody{}
	client := NewClient(&http.Client{Transport: timingTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Request-ID") != "" {
			t.Fatal("internal correlation leaked upstream")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}, &fakeTokenProvider{baseURL: "https://customer-secret.example"})
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	ctx := requesttiming.Start(context.Background(), uuid.New(), requesttiming.Core, requesttiming.Issue)
	if err := client.DoJSON(ctx, uuid.New(), http.MethodGet, "/api/v4/tasks?secret=query", nil, nil); err == nil {
		t.Fatal("read failure swallowed")
	}
	s := requesttiming.Snapshot(ctx)
	found := false
	for i := 0; i < s.Count; i++ {
		c := s.Cells[i]
		if c.Stage == requesttiming.AmoHTTP {
			found = true
			if c.Outcome != requesttiming.Failed || c.Micros < 20000 || !body.closed {
				t.Fatalf("body lifetime omitted: %+v", c)
			}
		}
	}
	if !found || strings.Contains(s.ServerTiming(), "secret") {
		t.Fatalf("missing or unsafe report: %s", s.ServerTiming())
	}
}
func TestTimingCountsLimiterRefusalAndHTTPFailure(t *testing.T) {
	for _, statusCode := range []int{http.StatusTooManyRequests, http.StatusNotModified} {
		client := NewClient(&http.Client{Transport: timingTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: statusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}, &fakeTokenProvider{baseURL: "https://example.test"})
		client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
		ctx := requesttiming.Start(context.Background(), uuid.New(), requesttiming.Gateway, requesttiming.OwnTask)
		if err := client.DoJSON(ctx, uuid.New(), http.MethodGet, "/api/v4/tasks", nil, nil); err == nil {
			t.Fatalf("HTTP %d swallowed", statusCode)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := client.waitBudget(canceled, AccessToken{AccountID: 42}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		s := requesttiming.Snapshot(ctx)
		var failed, canceledWait bool
		for i := 0; i < s.Count; i++ {
			c := s.Cells[i]
			failed = failed || c.Stage == requesttiming.AmoHTTP && c.Outcome == requesttiming.Failed
			canceledWait = canceledWait || c.Stage == requesttiming.Limiter && c.Outcome == requesttiming.Canceled
		}
		if !failed || !canceledWait || !s.Configs[requesttiming.Gateway].Present {
			t.Fatalf("missing failure or effective limits: %+v", s)
		}
	}
}
