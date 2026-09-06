package amocrm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// This uses the production 7/s, burst-7 limiter. All four callers must consume
// the same integration budget, including the prepared mutation path that does
// not go through DoJSON. Independent clients would incorrectly finish in burst.
func TestSharedBudgetAcrossExistingWidgetsAndEvents(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/events":
			_, _ = w.Write([]byte(`{"_embedded":{"events":[]}}`))
		case "/api/v4/users/22":
			_, _ = w.Write([]byte(`{"id":22,"rights":{"is_admin":true,"is_active":true}}`))
		case "/api/v4/leads/11":
			_, _ = w.Write([]byte(`{"id":11,"pipeline_id":1,"status_id":2}`))
		default:
			t.Errorf("unexpected API path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	provider := &fakeTokenProvider{baseURL: server.URL}
	client := NewClient(server.Client(), provider)
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	registry := prometheus.NewRegistry()
	client.SetMetrics(NewMetrics(registry))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mutation, err := client.PrepareLeadStatus(ctx, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 28)
	var wg sync.WaitGroup
	for i := 0; i < 28; i++ {
		wg.Add(1)
		go func(kind int) {
			defer wg.Done()
			<-start
			id := uuid.New()
			var callErr error
			switch kind % 4 {
			case 0:
				_, callErr = client.ListEvents(ctx, id, 1, 10, 1, 100)
			case 1:
				_, callErr = client.GetUserAuthorization(ctx, id, 22)
			case 2:
				_, callErr = client.GetLeadState(ctx, id, 11)
			case 3:
				callErr = mutation.SetLeadStatus(ctx, 11, 1, 2)
			}
			errs <- callErr
		}(i)
	}
	began := time.Now()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	times := append([]time.Time(nil), arrivals...)
	mu.Unlock()
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	if len(times) != 28 {
		t.Fatalf("HTTP requests=%d", len(times))
	}
	for i, at := range times {
		if i >= 7 {
			minimum := time.Duration(float64(i-6) / 7 * float64(time.Second))
			if elapsed := at.Sub(began); elapsed < minimum-40*time.Millisecond {
				t.Fatalf("aggregate burst/rate bypass: request=%d elapsed=%s minimum=%s", i+1, elapsed, minimum)
			}
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if actual := counterOutcome(families, "amocrm_requests_total", "2xx"); actual != 28 {
		t.Fatalf("request metric=%v", actual)
	}
	if actual := histogramOutcome(families, "amocrm_budget_wait_seconds", "admitted"); actual != 28 {
		t.Fatalf("admitted metric=%d", actual)
	}
	assertFiniteLabels(t, families)
}

func TestBudgetCancellationAnd429MetricsDoNotAddUnboundedLabels(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewClient(server.Client(), &fakeTokenProvider{baseURL: server.URL})
	client.resolveAccount = func(raw string) (*url.URL, error) { return url.Parse(raw) }
	registry := prometheus.NewRegistry()
	client.SetMetrics(NewMetrics(registry))
	_, err := client.ListEvents(context.Background(), uuid.New(), 1, 10, 1, 100)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrorRateLimited || apiErr.RetryAfter != 3*time.Second || !apiErr.Retryable {
		t.Fatalf("429 semantics: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.GetUserAuthorization(ctx, uuid.New(), 22); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("canceled or 429 call made extra requests: %d", requests.Load())
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if counterOutcome(families, "amocrm_requests_total", "429") != 1 || histogramOutcome(families, "amocrm_budget_wait_seconds", "canceled") != 1 {
		t.Fatal("429/cancellation not observed")
	}
	assertFiniteLabels(t, families)
}
func TestBudgetMetricsTransportFailureCategory(t *testing.T) {
	client := NewClient(&http.Client{Transport: budgetFailureTransport{}}, &fakeTokenProvider{baseURL: "https://test.amocrm.ru"})
	registry := prometheus.NewRegistry()
	client.SetMetrics(NewMetrics(registry))
	if err := client.DoJSON(context.Background(), uuid.New(), http.MethodGet, "/api/v4/account", nil, nil); err == nil {
		t.Fatal("expected transport failure")
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if counterOutcome(families, "amocrm_requests_total", "transport_error") != 1 {
		t.Fatal("transport failure not observed")
	}
	assertFiniteLabels(t, families)
}

type budgetFailureTransport struct{}

func (budgetFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("injected network outage")
}
func counterOutcome(families []*dto.MetricFamily, name, outcome string) float64 {
	for _, f := range families {
		if f.GetName() == name {
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "outcome" && l.GetValue() == outcome {
						return m.Counter.GetValue()
					}
				}
			}
		}
	}
	return 0
}
func histogramOutcome(families []*dto.MetricFamily, name, outcome string) uint64 {
	for _, f := range families {
		if f.GetName() == name {
			for _, m := range f.Metric {
				for _, l := range m.Label {
					if l.GetName() == "outcome" && l.GetValue() == outcome {
						return m.Histogram.GetSampleCount()
					}
				}
			}
		}
	}
	return 0
}
func assertFiniteLabels(t *testing.T, families []*dto.MetricFamily) {
	t.Helper()
	allowed := map[string]bool{"admitted": true, "canceled": true, "other": true, "transport_error": true, "429": true, "401": true, "2xx": true, "4xx": true, "5xx": true}
	for _, f := range families {
		for _, m := range f.Metric {
			for _, l := range m.Label {
				if l.GetName() != "outcome" || !allowed[l.GetValue()] {
					t.Fatalf("unbounded metric label: %s=%s", l.GetName(), l.GetValue())
				}
			}
		}
	}
}
