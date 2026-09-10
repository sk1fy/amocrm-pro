package crmevents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"golang.org/x/time/rate"
)

// L-01 uses the production scheduler/collector/enrichment loops, with a bounded
// external fixture shared by five installations. Initial timestamps are only
// setup; the report observes completed calls and newly ingested event details.
func TestStage6WorkersUnderSharedBudget(t *testing.T) {
	pool := eventsPool(t)
	cfg := DefaultConfig()
	cfg.PollInterval, cfg.Overlap = time.Second, time.Second
	store := NewPostgres(pool, cfg)
	policy := stage6LoadPolicy{principals: map[string]serviceapi.Principal{}}
	gw := &stage6LoadGateway{rel02Gateway: &rel02Gateway{}, events: map[string][]serviceapi.Event{}, calls: map[string]int{}, limiter: rate.NewLimiter(20, 1)}
	svc := NewWithRepository(store, policy, gw, cfg)
	full := os.Getenv("STAGE6_REL02_MEASURE") == "true"
	waves, history := 2, 20
	if full {
		waves, history = 6, 200
	}
	base := time.Now().UTC().Truncate(time.Second)
	var sources []rel02Source
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		src := rel02InsertSource(t, pool, base.Add(-2*time.Second))
		sources = append(sources, src)
		id := src.Principal.InstallationID.String()
		policy.principals[id] = src.Principal
		for j := 0; j < history; j++ {
			task := int64(1000 + j)
			gw.events[id] = append(gw.events[id], stage6TaskEvent(fmt.Sprintf("backfill-%d", j), task, base.Add(-15*time.Minute)))
		}
		// A previously fetched, linked task is due for a real refresh request.
		if _, err := pool.Exec(ctx, `INSERT INTO crm_events(installation_id,event_id,created_at,created_by,event_type,entity_id,entity_type,content_hash) VALUES($1,'refresh',now()-interval '1 hour',77,'task_added',1,'task','hash'::bytea)`, src.Principal.InstallationID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,object_id,state,source,payload,fetched_at,run_after) VALUES($1,'task','1',1,'ready','tasks_api','{"id":1}',now()-interval '1 hour',now()-interval '1 minute')`, src.Principal.InstallationID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO event_enrichment_links(installation_id,event_id,object_kind,object_key) VALUES($1,'refresh','task','1')`, src.Principal.InstallationID); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Apply(ctx, serviceapi.Command{Auth: serviceapi.Auth{Token: id}, CommandID: "load-backfill", Kind: "backfill", From: base.Add(-20 * time.Minute).Unix(), To: base.Add(-10 * time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	type observation struct {
		ElapsedMS     int64            `json:"elapsed_ms"`
		Jobs          map[string]int64 `json:"jobs"`
		OldestJob     float64          `json:"oldest_job_seconds"`
		EnrichmentAge float64          `json:"enrichment_age_seconds"`
		Lag           float64          `json:"collector_lag_seconds"`
		Ready         int              `json:"new_details_ready"`
	}
	started := time.Now()
	issued, ready := map[string]time.Time{}, map[string]bool{}
	var delays, rawDelays []time.Duration
	seen := map[string]bool{}
	var series []observation
	var perInstallation = map[string]int{}
	nextWave := started
	wave := 0
	deadline := started.Add(30 * time.Second)
	var backfills, pending, invalid int
	for time.Now().Before(deadline) {
		if wave < waves && !time.Now().Before(nextWave) {
			for _, src := range sources {
				installation := src.Principal.InstallationID.String()
				eventID := fmt.Sprintf("current-%d", wave)
				at := time.Now()
				issued[installation+"/"+eventID] = at
				gw.mu.Lock()
				gw.events[installation] = append(gw.events[installation], stage6TaskEvent(eventID, int64(100000+wave), at))
				gw.mu.Unlock()
			}
			wave++
			nextWave = nextWave.Add(time.Second)
		}
		rows, err := pool.Query(ctx, `SELECT e.installation_id::text,e.event_id,o.state='ready' FROM crm_events e JOIN event_enrichment_links l USING(installation_id,event_id) JOIN event_enrichment_objects o USING(installation_id,object_kind,object_key) WHERE e.event_id LIKE 'current-%'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var installation, eventID string
			var enriched bool
			if err := rows.Scan(&installation, &eventID, &enriched); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			key := installation + "/" + eventID
			if !seen[key] {
				rawDelays = append(rawDelays, time.Since(issued[key]))
				seen[key] = true
			}
			if enriched && !ready[key] {
				delays = append(delays, time.Since(issued[key]))
				ready[key] = true
				perInstallation[installation]++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.MetricsSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		series = append(series, observation{time.Since(started).Milliseconds(), snapshot.States, snapshot.AgeSeconds, snapshot.EnrichmentAgeSeconds, snapshot.LagSeconds, len(ready)})
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_jobs WHERE kind='backfill' AND status='completed'),(SELECT count(*) FROM event_enrichment_objects o WHERE `+enrichmentClaimable+` AND o.run_after<=now())`).Scan(&backfills, &pending); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_enrichment_objects WHERE state <> 'ready'`).Scan(&invalid); err != nil {
			t.Fatal(err)
		}
		if wave == waves && len(ready) == waves*len(sources) && backfills == len(sources) && pending == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(ready) != waves*len(sources) || backfills != 5 || pending != 0 || invalid != 0 {
		t.Errorf("work did not drain within 30s: details=%d/%d backfills=%d pending=%d not_ready=%d", len(ready), waves*5, backfills, pending, invalid)
	}
	for _, src := range sources {
		if perInstallation[src.Principal.InstallationID.String()] != waves {
			t.Errorf("installation %s details=%d want %d", src.Principal.InstallationID, perInstallation[src.Principal.InstallationID.String()], waves)
		}
	}
	lat, raw := rel02Percentiles(delays), rel02Percentiles(rawDelays)
	if lat.p95 > 15*time.Second || lat.p99 > 20*time.Second {
		t.Errorf("details exceed fixed thresholds p95=%s p99=%s", lat.p95, lat.p99)
	}
	var maxLag float64
	for _, point := range series {
		if point.Lag > maxLag {
			maxLag = point.Lag
		}
	}
	if maxLag > 15 {
		t.Errorf("collector lag %.3fs exceeds 15s", maxLag)
	}
	gw.mu.Lock()
	counts := map[string]int{}
	for name, n := range gw.calls {
		counts[name] = n
	}
	waits := append([]time.Duration{}, gw.waits...)
	refreshObjects, taskObjects := gw.refreshObjects, gw.taskObjects
	gw.mu.Unlock()
	if counts["events"] == 0 || counts["tasks"] == 0 || counts["refresh_requests"] == 0 || refreshObjects < 5 {
		t.Errorf("fixture did not exercise all paths: %+v refresh=%d", counts, refreshObjects)
	}
	waitLat := rel02Percentiles(waits)
	result := map[string]any{"installations": 5, "workers": cfg.Workers, "waves": waves, "new_events": waves * 5, "backfill_events": history * 5, "completed_backfills": backfills, "budget_requests_per_second": 20, "external_latency_ms": 20, "elapsed": time.Since(started).String(), "calls": counts, "refresh_objects": refreshObjects, "task_objects": taskObjects, "refresh_share_of_task_requests": float64(counts["refresh_requests"]) / float64(max(1, counts["tasks"])), "detail_ready_p95": lat.p95.String(), "detail_ready_p99": lat.p99.String(), "raw_event_p95": raw.p95.String(), "raw_event_p99": raw.p99.String(), "max_collector_lag_seconds": maxLag, "budget_wait_p95": waitLat.p95.String(), "budget_wait_p99": waitLat.p99.String(), "series": series}
	if full {
		rel02WriteJSON(t, filepath.Join(rel02ArtifactDir(t), "workers.json"), result)
	}
	t.Logf("live workers: events=%d tasks=%d refresh_requests=%d details=%d p95=%s p99=%s lag_max=%.3fs elapsed=%s", counts["events"], counts["tasks"], counts["refresh_requests"], len(ready), lat.p95, lat.p99, maxLag, time.Since(started))
}

func stage6TaskEvent(id string, task int64, at time.Time) serviceapi.Event {
	return serviceapi.Event{ID: id, CreatedAt: at.Unix(), CreatedBy: 77, Type: "task_added", EntityID: task, EntityType: "task", ValueBefore: json.RawMessage(`[]`), ValueAfter: json.RawMessage(`[]`)}
}

type stage6LoadPolicy struct {
	principals map[string]serviceapi.Principal
}

func (p stage6LoadPolicy) Issue(_ context.Context, r serviceapi.IssueRequest) (serviceapi.Auth, error) {
	if principal, ok := p.principals[r.InstallationID.String()]; !ok || principal.Scope != r.Scope {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.PermissionDenied, "fixture scope")
	}
	return serviceapi.Auth{Token: r.InstallationID.String()}, nil
}
func (p stage6LoadPolicy) Validate(_ context.Context, a serviceapi.Auth, _ string, _ string) (serviceapi.Principal, error) {
	if principal, ok := p.principals[a.Token]; ok {
		return principal, nil
	}
	return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "fixture scope")
}

type stage6LoadGateway struct {
	*rel02Gateway
	mu                          sync.Mutex
	events                      map[string][]serviceapi.Event
	calls                       map[string]int
	waits                       []time.Duration
	refreshObjects, taskObjects int
	limiter                     *rate.Limiter
}

func (g *stage6LoadGateway) admit(ctx context.Context, kind string) error {
	start := time.Now()
	if err := g.limiter.Wait(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	g.waits = append(g.waits, time.Since(start))
	g.calls[kind]++
	g.mu.Unlock()
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (g *stage6LoadGateway) Events(ctx context.Context, r serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	if err := g.admit(ctx, "events"); err != nil {
		return serviceapi.EventPage{}, err
	}
	g.mu.Lock()
	var matching []serviceapi.Event
	for _, e := range g.events[r.Auth.Token] {
		if e.CreatedAt >= r.From && e.CreatedAt <= r.To {
			matching = append(matching, e)
		}
	}
	g.mu.Unlock()
	sort.Slice(matching, func(i, j int) bool {
		if matching[i].CreatedAt == matching[j].CreatedAt {
			return matching[i].ID < matching[j].ID
		}
		return matching[i].CreatedAt < matching[j].CreatedAt
	})
	from := min((r.Page-1)*r.Limit, len(matching))
	to := min(from+r.Limit, len(matching))
	return serviceapi.EventPage{Events: matching[from:to], HasNext: to < len(matching)}, nil
}
func (g *stage6LoadGateway) Tasks(ctx context.Context, r serviceapi.TasksRequest) (serviceapi.TaskPage, error) {
	if err := g.admit(ctx, "tasks"); err != nil {
		return serviceapi.TaskPage{}, err
	}
	page := serviceapi.TaskPage{}
	refresh := 0
	for _, id := range r.IDs {
		if id == 1 {
			refresh++
		}
		page.Tasks = append(page.Tasks, serviceapi.Task{ID: id, Text: "Synthetic task " + strconv.FormatInt(id, 10), UpdatedAt: time.Now().Unix()})
	}
	g.mu.Lock()
	g.taskObjects += len(r.IDs)
	g.refreshObjects += refresh
	if refresh > 0 {
		g.calls["refresh_requests"]++
	}
	g.mu.Unlock()
	return page, nil
}
