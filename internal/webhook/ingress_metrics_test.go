package webhook

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestIngressLimiterMetricsUseOnlyBoundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewIngressMetrics(registry)
	metrics.observeDecision(ingressScopeGlobal, true)
	metrics.observeDecision(ingressScopeGlobal, false)
	metrics.observeDecision(ingressScopeInstallation, true)
	metrics.observeDecision(ingressScopeInstallation, false)
	metrics.addEntry()
	metrics.evictEntries(1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "scope" && label.GetName() != "outcome" {
					t.Fatalf("unexpected limiter metric label %q", label.GetName())
				}
				if label.GetName() == "scope" && label.GetValue() != ingressScopeGlobal &&
					label.GetValue() != ingressScopeInstallation {
					t.Fatalf("unexpected scope label %q", label.GetValue())
				}
				if label.GetName() == "outcome" && label.GetValue() != ingressOutcomeAllowed &&
					label.GetValue() != ingressOutcomeRejected {
					t.Fatalf("unexpected outcome label %q", label.GetValue())
				}
			}
		}
	}
}
