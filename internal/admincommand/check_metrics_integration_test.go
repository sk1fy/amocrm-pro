package admincommand

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"testing"
	"time"
)

func TestCheckPopulationMetricsDoNotTreatUnknownAsVerified(t *testing.T) {
	_, pool, _, _ := fixture(t)
	m := NewCheckMetrics(prometheus.NewRegistry())
	pooled, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn := pooled.Hijack()
	defer closeCheckSession(conn)
	if err := m.Refresh(t.Context(), conn, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(m.Population.WithLabelValues("unknown")); got != 1 {
		t.Fatalf("unknown=%v", got)
	}
	if got := testutil.ToFloat64(m.Population.WithLabelValues("fresh")); got != 0 {
		t.Fatalf("unknown became verified=%v", got)
	}
}
