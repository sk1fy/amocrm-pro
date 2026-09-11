package activitybridge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/apicontract"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func TestUnavailableDeliveryMetricsDoNotPublishZeroBacklog(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister((&Bridge{}).Collector())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "activity_delivery_metrics_up" || families[0].Metric[0].Gauge.GetValue() != 0 {
		t.Fatalf("misleading outage snapshot: %+v", families)
	}
}

func TestHTTPRouteClassIsBounded(t *testing.T) {
	if httpRouteClass(apicontract.ActivityPanel.Path) != "panel" || httpRouteClass(apicontract.ActivityEvent.Path) != "event" {
		t.Fatal("panel/event classes")
	}
	if httpRouteClass(apicontract.ActivityStatus.Path) != "other" || httpRouteClass("/api/v1/widget/activity/events/secret-id") != "other" {
		t.Fatal("paths must not become labels")
	}
}

func TestHTTPResponseSizeUsesBoundedRouteClassAndIncludesCapBucket(t *testing.T) {
	bridge := New(nil, nil, nil, nil)
	router := chi.NewRouter()
	bridge.RegisterHTTP(router, func(h http.Handler) http.Handler { return h }, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apicontract.ActivityPanel.Path, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(bridge.Collector())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sampleCount uint64
	foundCap := false
	for _, family := range families {
		if family.GetName() != "activity_http_response_bytes" {
			continue
		}
		if len(family.Metric) != 1 {
			t.Fatalf("response size series=%d", len(family.Metric))
		}
		metric := family.Metric[0]
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "route" || metric.Label[0].GetValue() != "panel" {
			t.Fatalf("labels=%v", metric.Label)
		}
		sampleCount = metric.Histogram.GetSampleCount()
		for _, bucket := range metric.Histogram.Bucket {
			if bucket.GetUpperBound() == float64(serviceapi.MaxResponseBytes) {
				foundCap = true
			}
		}
	}
	if sampleCount != 1 {
		t.Fatalf("panel observations=%d", sampleCount)
	}
	if !foundCap {
		t.Fatal("missing 3 MiB cap bucket")
	}
}

func TestHTTPResponseSizeDoesNotLogBodies(t *testing.T) {
	bridge := New(nil, nil, nil, nil)
	handler := bridge.observeSize(apicontract.ActivityEvent, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"secret":"do-not-scrape"}`)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apicontract.ActivityEvent.Path, nil))
	if !strings.Contains(rec.Body.String(), "do-not-scrape") {
		t.Fatal("handler body missing")
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(bridge.responseSize)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].GetName() != "activity_http_response_bytes" {
		t.Fatalf("families=%v", families)
	}
	if strings.Contains(families[0].String(), "do-not-scrape") {
		t.Fatal("payload leaked into metrics")
	}
	metric := families[0].Metric[0]
	if metric.Label[0].GetValue() != "event" || metric.Histogram.GetSampleCount() != 1 || metric.Histogram.GetSampleSum() != float64(len(`{"secret":"do-not-scrape"}`)) {
		t.Fatalf("size metric=%v", metric)
	}
}
