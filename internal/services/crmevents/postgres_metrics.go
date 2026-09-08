package crmevents

import "context"

// MetricsSnapshot reads only the owner database; labels are normalized before
// crossing the metrics port even if an operator introduced an unknown DB state.
func (s *Postgres) MetricsSnapshot(ctx context.Context) (Snapshot, error) {
	result := Snapshot{States: map[string]int64{}, EnrichmentStates: map[string]int64{}}
	rows, err := s.pool.Query(ctx, `SELECT status,count(*) FROM event_jobs GROUP BY status`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var status string
		var n int64
		if err = rows.Scan(&status, &n); err != nil {
			break
		}
		result.States[metricState(status)] += n
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	err = s.pool.QueryRow(ctx, `SELECT coalesce(extract(epoch from now()-min(created_at) FILTER(WHERE status IN ('queued','running','retry','paused'))),0),coalesce(sum(processed),0),coalesce(sum(inserted),0),coalesce(sum(updated),0),coalesce(sum(deduplicated),0) FROM event_jobs`).Scan(&result.AgeSeconds, &result.Processed, &result.Inserted, &result.Updated, &result.Deduplicated)
	if err != nil {
		return result, err
	}
	if err = s.pool.QueryRow(ctx, `SELECT coalesce(max(extract(epoch from now()-continuous_to)),0) FROM event_sources s WHERE EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=s.installation_id AND c.enabled)`).Scan(&result.LagSeconds); err != nil {
		return result, err
	}
	rows, err = s.pool.Query(ctx, `SELECT state,count(*) FROM event_enrichment_objects GROUP BY state`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var state string
		var n int64
		if err = rows.Scan(&state, &n); err != nil {
			break
		}
		result.EnrichmentStates[enrichmentMetricState(state)] += n
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	err = s.pool.QueryRow(ctx, `SELECT coalesce(extract(epoch from now()-min(updated_at) FILTER(WHERE state IN ('pending','retry'))),0) FROM event_enrichment_objects`).Scan(&result.EnrichmentAgeSeconds)
	return result, err
}
