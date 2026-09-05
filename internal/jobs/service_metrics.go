package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

// Explicit types only: arbitrary persisted job names never become labels.
func metricJobServices() map[string]string {
	result := services.JobTypes()
	for jobType, service := range result {
		if service == "" {
			result[jobType] = "platform"
		}
	}
	for _, jobType := range []string{"webhook.parse", "webhook.process_event", "webhook.reconcile"} {
		result[jobType] = "platform"
	}
	return result
}

func metricService(jobType string) string {
	if service, known := services.JobService(jobType); known {
		if service == "" {
			return "platform"
		}
		return service
	}
	switch jobType {
	case "webhook.parse", "webhook.process_event", "webhook.reconcile":
		return "platform"
	}
	return "other"
}

type ServiceBacklogCollector struct {
	queryer  backlogQueryer
	timeout  time.Duration
	backlog  *prometheus.Desc
	oldest   *prometheus.Desc
	mapping  []byte
	services []string
}

func NewServiceBacklogCollector(queryer backlogQueryer, timeout time.Duration) (*ServiceBacklogCollector, error) {
	if queryer == nil {
		return nil, errors.New("service backlog queryer is required")
	}
	if timeout <= 0 {
		return nil, errors.New("service backlog collection timeout must be positive")
	}
	mapping := metricJobServices()
	encoded, err := json.Marshal(mapping)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{"other": true, "platform": true}
	for _, name := range mapping {
		names[name] = true
	}
	bounded := make([]string, 0, len(names))
	for name := range names {
		bounded = append(bounded, name)
	}
	sort.Strings(bounded)
	return &ServiceBacklogCollector{queryer: queryer, timeout: timeout, mapping: encoded, services: bounded,
		backlog: prometheus.NewDesc("amocrm_jobs_service_backlog", "Exact PostgreSQL job backlog by bounded service and eligibility kind.", []string{"service", "kind"}, nil),
		oldest:  prometheus.NewDesc("amocrm_jobs_service_oldest_ready_seconds", "Age since run_after of the oldest eligible ready job by bounded service; zero when empty.", []string{"service"}, nil),
	}, nil
}
func (c *ServiceBacklogCollector) Describe(out chan<- *prometheus.Desc) {
	out <- c.backlog
	out <- c.oldest
}
func (c *ServiceBacklogCollector) Collect(out chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	var encoded []byte
	if err := c.queryer.QueryRow(ctx, serviceBacklogQuery, c.mapping).Scan(&encoded); err != nil {
		out <- prometheus.NewInvalidMetric(c.backlog, err)
		return
	}
	type counts struct {
		Ready      int64   `json:"ready"`
		Scheduled  int64   `json:"scheduled"`
		Processing int64   `json:"processing"`
		Expired    int64   `json:"expired"`
		Oldest     float64 `json:"oldest"`
	}
	var values map[string]counts
	if err := json.Unmarshal(encoded, &values); err != nil {
		out <- prometheus.NewInvalidMetric(c.backlog, err)
		return
	}
	for _, service := range c.services {
		value := values[service]
		for _, kind := range []struct {
			name  string
			count int64
		}{{"ready", value.Ready}, {"scheduled", value.Scheduled}, {"processing", value.Processing}, {"expired_lease", value.Expired}} {
			out <- prometheus.MustNewConstMetric(c.backlog, prometheus.GaugeValue, float64(kind.count), service, kind.name)
		}
		out <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, value.Oldest, service)
	}
}

const serviceBacklogQuery = `
WITH grouped AS (
    SELECT COALESCE($1::jsonb ->> type, 'other') AS service,
        count(*) FILTER (WHERE status IN ('queued','retry') AND attempts<max_attempts AND run_after<=statement_timestamp()) AS ready,
        count(*) FILTER (WHERE status IN ('queued','retry') AND attempts<max_attempts AND run_after>statement_timestamp()) AS scheduled,
        count(*) FILTER (WHERE status='processing' AND locked_until>=statement_timestamp()) AS processing,
        count(*) FILTER (WHERE status='processing' AND locked_until<statement_timestamp()) AS expired,
        COALESCE(EXTRACT(EPOCH FROM (statement_timestamp() - min(run_after) FILTER (
            WHERE status IN ('queued','retry') AND attempts<max_attempts AND run_after<=statement_timestamp()))),0) AS oldest
    FROM jobs WHERE status IN ('queued','retry','processing')
    GROUP BY COALESCE($1::jsonb ->> type, 'other')
)
SELECT COALESCE(jsonb_object_agg(service,to_jsonb(grouped)-'service'),'{}'::jsonb) FROM grouped`
