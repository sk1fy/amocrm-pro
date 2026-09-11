package crmevents

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc"
	"github.com/sk1fy/amocrm-pro/internal/services/activity"
)

// Thresholds are copied from docs/verification/stage6-2026-09-08/rel-02.md
// and must not be raised to make a run green.
const (
	rel02Warmup            = 3
	rel02Samples           = 25
	rel02QueryP95          = time.Second
	rel02QueryP99          = 2 * time.Second
	rel02CardP95           = 200 * time.Millisecond
	rel02CardP99           = 500 * time.Millisecond
	rel02ClaimP95          = 100 * time.Millisecond
	rel02ClaimP99          = 300 * time.Millisecond
	rel02PanelP95          = 1200 * time.Millisecond
	rel02PanelP99          = 2500 * time.Millisecond
	rel02EventCardP95      = 300 * time.Millisecond
	rel02EventCardP99      = 700 * time.Millisecond
	rel02ExplainHistory    = 200 * time.Millisecond
	rel02ExplainAggregate  = 400 * time.Millisecond
	rel02ExplainLookup     = 20 * time.Millisecond
	rel02ExplainEnrichment = 30 * time.Millisecond
	rel02ExplainClaim      = 100 * time.Millisecond
	rel02PoolAcquireP95    = 100 * time.Millisecond
	rel02SeqScanMinRows    = 10000
	rel02HighSharedRead    = 10000
	rel02HighHeapFetches   = 10000
)

var (
	rel02ExecRe  = regexp.MustCompile(`Execution Time: ([0-9.]+) ms`)
	rel02HitRe   = regexp.MustCompile(`shared hit=(\d+)`)
	rel02ReadRe  = regexp.MustCompile(`shared hit=\d+ read=(\d+)`)
	rel02HeapRe  = regexp.MustCompile(`Heap Fetches: (\d+)`)
	rel02SeqRe   = regexp.MustCompile(`(?i)(?:Parallel )?Seq Scan on ([a-z0-9_]+)`)
	rel02IndexRe = regexp.MustCompile(`(?i)(?:Index(?: Only)? Scan using |Bitmap Index Scan on )([a-z0-9_]+)`)
)

type rel02Kind struct {
	Type, EntityType string
}

var rel02Types = []rel02Kind{
	{"task_completed", "task"},
	{"incoming_call", "lead"},
	{"common_note_added", "lead"},
	{"outgoing_chat_message", "lead"},
	{"lead_status_changed", "lead"},
	{"sale_field_changed", "lead"},
	{"entity_responsible_changed", "lead"},
	{"custom_field_101_value_changed", "lead"},
	{"contact_linked", "lead"},
	{"attachment_note_added", "lead"},
	{"lead_added", "lead"},
}

type rel02Profile struct {
	ID, Buckets                         string
	Users, Days, PerUserDay, LargeNotes int
	UnknownAuthors                      bool
	Jobs, Enrichment                    bool
	Installations                       int
}

func TestStage6ReadMeasure(t *testing.T) {
	if os.Getenv("STAGE6_REL02_MEASURE") != "true" {
		rel02CISmoke(t)
		return
	}
	pool := eventsPool(t)
	store := NewPostgres(pool, DefaultConfig())
	dir := rel02ArtifactDir(t)
	report := &rel02Report{Thresholds: rel02ThresholdMap(), Started: time.Now().UTC()}
	rel02WriteJSON(t, filepath.Join(dir, "thresholds.json"), report.Thresholds)
	rel02WriteJSON(t, filepath.Join(dir, "stand.json"), rel02Stand(t, pool))

	profiles := []rel02Profile{
		{ID: "small_department", Users: 8, Days: 7, PerUserDay: 40, Buckets: serviceapi.BucketDay},
		{ID: "many_employees", Users: 100, Days: 7, PerUserDay: 50, Buckets: serviceapi.BucketDay, Enrichment: true},
		{ID: "dense_day", Users: 40, Days: 1, PerUserDay: 400, Buckets: serviceapi.BucketHour},
		{ID: "large_notes", Users: 12, Days: 1, LargeNotes: 200, Buckets: serviceapi.BucketHour},
		{ID: "long_backfill", Users: 16, Days: 30, PerUserDay: 30, Buckets: serviceapi.BucketDay, Jobs: true},
		{ID: "multi_install_read", Users: 8, Days: 2, PerUserDay: 20, Buckets: serviceapi.BucketDay, UnknownAuthors: true, Jobs: true, Enrichment: true, Installations: 5},
	}
	for _, profile := range profiles {
		t.Run(profile.ID, func(t *testing.T) {
			rel02RunProfile(t, pool, store, dir, report, profile)
		})
	}
	report.Finished = time.Now().UTC()
	report.Elapsed = report.Finished.Sub(report.Started).String()
	rel02WriteJSON(t, filepath.Join(dir, "summary.json"), report)
	if len(report.Failures) > 0 {
		t.Fatalf("%d threshold/plan failures; see %s", len(report.Failures), dir)
	}
}

// Cheap always-on path so activity-ci does not SKIP. Full volume profiles stay
// behind STAGE6_REL02_MEASURE=true and are not a production load claim.
func rel02CISmoke(t *testing.T) {
	t.Helper()
	pool := eventsPool(t)
	store := NewPostgres(pool, DefaultConfig())
	src := rel02InsertSource(t, pool, time.Now().UTC().Add(-time.Minute))
	src.Users = rel02Users(4)
	src.From, src.To = rel02Window(1)
	rel02CopyEvents(t, pool, src, 40, []byte(`[{"id":1}]`), false)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ANALYZE crm_events`); err != nil {
		t.Fatal(err)
	}
	q := serviceapi.Query{From: src.From, To: src.To, Limit: 100, Compact: true, Timezone: "UTC", UserIDs: rel02UserIDs(src.Users)}
	var timeIdx, userIdx int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_indexes WHERE indexname='crm_events_time'),(SELECT count(*) FROM pg_indexes WHERE indexname='crm_events_user_time')`).Scan(&timeIdx, &userIdx); err != nil || timeIdx != 1 || userIdx != 1 {
		t.Fatalf("history indexes missing time=%d user=%d err=%v", timeIdx, userIdx, err)
	}
	result, err := store.Query(ctx, q, src.Principal)
	if err != nil || len(result.Events) == 0 {
		t.Fatalf("CI smoke query events=%d err=%v", len(result.Events), err)
	}
	id := rel02AnyEventID(t, pool, src.Principal.InstallationID)
	if _, err = store.GetEvent(ctx, src.Principal, id); err != nil {
		t.Fatalf("CI smoke GetEvent: %v", err)
	}
}

func rel02RunProfile(t *testing.T, pool *pgxpool.Pool, store *Postgres, dir string, report *rel02Report, profile rel02Profile) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if _, err := pool.Exec(ctx, `TRUNCATE event_sources CASCADE`); err != nil {
		t.Fatal(err)
	}
	installs := profile.Installations
	if installs < 1 {
		installs = 1
	}
	sources := make([]rel02Source, installs)
	small := []byte(`[{"id":1}]`)
	var large []byte
	if profile.LargeNotes > 0 {
		large = []byte(`[{"text":"` + strings.Repeat("w", 32000) + `"}]`)
	}
	var rows int
	for i := 0; i < installs; i++ {
		src := rel02InsertSource(t, pool, time.Now().UTC().Add(-time.Duration(i+1)*7*time.Minute))
		src.Users = rel02Users(profile.Users)
		src.From, src.To = rel02Window(profile.Days)
		sources[i] = src
		n := profile.PerUserDay * profile.Users * profile.Days
		if profile.LargeNotes > 0 {
			n = profile.LargeNotes
		}
		if i > 0 {
			n = min(n, 400)
		}
		payload := small
		if profile.LargeNotes > 0 && i == 0 {
			payload = large
		}
		rel02CopyEvents(t, pool, src, n, payload, profile.UnknownAuthors && i == 0)
		rows += n
		if profile.Jobs {
			rel02SeedJobs(t, pool, src, i)
		}
		if profile.Enrichment {
			rel02SeedEnrichment(t, pool, src, n, i == 0)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE crm_events; ANALYZE event_jobs; ANALYZE event_enrichment_objects; ANALYZE event_sources; ANALYZE event_enrichment_links`); err != nil {
		t.Fatal(err)
	}
	primary := sources[0]
	gw := &rel02Gateway{users: primary.Users}
	svc := NewWithRepository(store, &testPolicy{principal: primary.Principal}, gw, DefaultConfig())
	q := serviceapi.Query{From: primary.From, To: primary.To, Limit: 100, Compact: true, Timezone: "UTC", Buckets: profile.Buckets, UserIDs: rel02UserIDs(primary.Users)}
	out := map[string]any{"profile": profile, "rows": rows, "installations": installs, "from": primary.From, "to": primary.To}

	if profile.ID == "many_employees" || profile.ID == "dense_day" || profile.ID == "small_department" || profile.ID == "long_backfill" || profile.ID == "multi_install_read" {
		out["explain"] = rel02ExplainProduct(t, pool, dir, report, profile, primary, q)
	}
	out["query_compact"] = rel02ProbeQuery(t, report, svc, gw, q, "Query compact "+profile.ID, rel02QueryP95, rel02QueryP99)
	if profile.LargeNotes == 0 {
		full := q
		full.Compact = false
		out["query_full"] = rel02ProbeQuery(t, report, svc, gw, full, "Query full "+profile.ID, rel02QueryP95, rel02QueryP99)
	}
	cardID := rel02AnyEventID(t, pool, primary.Principal.InstallationID)
	out["get_event"] = rel02ProbeCard(t, report, svc, gw, cardID, "GetEvent "+profile.ID, rel02CardP95, rel02CardP99)
	out["sizes"] = rel02MeasureSizes(t, report, svc, gw, primary, q, cardID, profile.ID)
	if profile.Jobs {
		out["claim"] = rel02ProbeClaim(t, report, store, pool)
	}
	if profile.Enrichment {
		out["enrichment_claim"] = rel02ProbeEnrichmentClaim(t, report, store, pool)
	}
	if profile.ID == "multi_install_read" {
		out["queue_snapshot"] = rel02QueueSnapshot(t, store, pool, gw, sources)
		rel02WriteJSON(t, filepath.Join(dir, "queue-snapshot.json"), out["queue_snapshot"])
	}
	stat := pool.Stat()
	out["pool"] = map[string]any{
		"acquired": stat.AcquiredConns(), "idle": stat.IdleConns(), "max": stat.MaxConns(),
		"empty_acquire": stat.EmptyAcquireCount(), "acquire_duration": stat.AcquireDuration().String(),
	}
	if stat.EmptyAcquireCount() > 0 {
		avg := time.Duration(int64(stat.AcquireDuration()) / int64(stat.AcquireCount()))
		if avg > rel02PoolAcquireP95 {
			report.fail(t, "%s pool acquire average %s exceeds P95 %s", profile.ID, avg, rel02PoolAcquireP95)
		}
	}
	out["amocrm"] = gw.snapshot()
	rel02WriteJSON(t, filepath.Join(dir, "profiles", profile.ID+".json"), out)
}

func rel02ExplainProduct(t *testing.T, pool *pgxpool.Pool, dir string, report *rel02Report, profile rel02Profile, src rel02Source, q serviceapi.Query) map[string]any {
	t.Helper()
	args := historyArgs(q, src.Principal)
	pageArgs := append(append([]any{}, args...), false, int64(0), "", q.Limit+1)
	projection := eventColumns + `,NULL::jsonb,NULL::jsonb`
	pageSQL := `SELECT ` + projection + eventHistoryFrom + ` AND ($13::bool=false OR (e.created_at,e.event_id)>(to_timestamp($14),$15)) ORDER BY e.created_at ASC,e.event_id ASC LIMIT $16`
	summarySQL := `SELECT e.created_by,count(*),coalesce(extract(epoch from min(e.created_at))::bigint,0),extract(epoch from max(e.created_at))::bigint,count(distinct (e.entity_type || ':' || e.entity_id::text)),count(*) FILTER (WHERE e.event_type='task_completed'),count(distinct e.entity_id) FILTER (WHERE e.event_type='task_completed')` + eventHistoryFrom + ` GROUP BY e.created_by ORDER BY e.created_by LIMIT 101`
	categorySQL := `SELECT e.created_by,` + eventCategorySQL("e.event_type") + `,count(*)` + eventHistoryFrom + ` GROUP BY 1,2 ORDER BY 1,2`
	totalsSQL := `SELECT count(*),count(distinct (e.entity_type || ':' || e.entity_id::text)),count(*) FILTER (WHERE e.event_type='task_completed'),count(distinct e.entity_id) FILTER (WHERE e.event_type='task_completed'),coalesce(extract(epoch from min(e.created_at))::bigint,0),coalesce(extract(epoch from max(e.created_at))::bigint,0)` + eventHistoryFrom
	buckets, err := serviceapi.QueryTimeBuckets(q)
	if err != nil {
		t.Fatal(err)
	}
	starts := make([]int64, len(buckets))
	for i, b := range buckets {
		starts[i] = b.StartAt
	}
	timelineSQL := `SELECT width_bucket(extract(epoch from e.created_at)::bigint,$13::bigint[]),count(*)` + eventHistoryFrom + ` GROUP BY 1`
	authorQ := q
	authorQ.UserIDs = rel02UserIDs(src.Users[:min(len(src.Users), 10)])
	typeQ := q
	typeQ.Types = []string{"task_completed"}
	prefixQ := q
	prefixQ.TypePrefix = "custom_field_"
	entityQ := q
	entityQ.EntityType = "lead"
	entityQ.EntityIDs = []int64{31001, 31002, 31003}
	catQ := q
	catQ.Categories = []string{serviceapi.CategoryTasks}
	unknownQ := q
	if len(src.Users) > 0 {
		unknownQ.UserIDs = rel02UserIDs(src.Users[:min(len(src.Users), 10)])
		unknownQ.DirectoryUserIDs = unknownQ.UserIDs
		unknownQ.IncludeUnknownAuthors = true
	}
	queries := []struct {
		name, sql string
		args      []any
		limit     time.Duration
		table     string
	}{
		{"history-compact", pageSQL, pageArgs, rel02ExplainHistory, "crm_events"},
		{"history-authors", pageSQL, append(append([]any{}, historyArgs(authorQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"history-types", pageSQL, append(append([]any{}, historyArgs(typeQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"history-prefix", pageSQL, append(append([]any{}, historyArgs(prefixQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"history-entity", pageSQL, append(append([]any{}, historyArgs(entityQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"history-category", pageSQL, append(append([]any{}, historyArgs(catQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"history-unknown-authors", pageSQL, append(append([]any{}, historyArgs(unknownQ, src.Principal)...), false, int64(0), "", q.Limit+1), rel02ExplainHistory, "crm_events"},
		{"summary", summarySQL, args, rel02ExplainAggregate, "crm_events"},
		{"category-counts", categorySQL, args, rel02ExplainAggregate, "crm_events"},
		{"totals", totalsSQL, args, rel02ExplainAggregate, "crm_events"},
		{"timeline", timelineSQL, append(append([]any{}, args...), starts), rel02ExplainAggregate, "crm_events"},
		{"get-event", `SELECT ` + eventColumns + `,e.value_before,e.value_after FROM crm_events e JOIN event_sources s ON s.installation_id=e.installation_id WHERE e.installation_id=$1 AND s.integration_id=$2 AND e.event_id=$3`, []any{src.Principal.InstallationID, src.Principal.IntegrationID, rel02AnyEventID(t, pool, src.Principal.InstallationID)}, rel02ExplainLookup, "crm_events"},
		{"load-enrichment", `SELECT o.object_kind,o.object_key,o.state,o.reason_code,o.source,coalesce(extract(epoch from o.fetched_at)::bigint,0),o.payload FROM event_enrichment_links l JOIN event_enrichment_objects o ON o.installation_id=l.installation_id AND o.object_kind=l.object_kind AND o.object_key=l.object_key WHERE l.installation_id=$1 AND l.event_id=$2 ORDER BY o.object_kind,o.object_key`, []any{src.Principal.InstallationID, rel02AnyEventID(t, pool, src.Principal.InstallationID)}, rel02ExplainEnrichment, "event_enrichment"},
	}
	if profile.Jobs || profile.ID == "multi_install_read" {
		queries = append(queries,
			struct {
				name, sql string
				args      []any
				limit     time.Duration
				table     string
			}{"claim-backfill-active", `SELECT EXISTS(SELECT 1 FROM event_jobs j JOIN event_sources s USING(installation_id) WHERE j.kind='backfill' AND j.status='running' AND s.lease_until>now())`, nil, rel02ExplainClaim, "event_jobs"},
			struct {
				name, sql string
				args      []any
				limit     time.Duration
				table     string
			}{"claim-source", `SELECT s.installation_id,s.integration_id FROM event_sources s WHERE s.state NOT IN ('reauth_required','disabled','paused','failed') AND (s.lease_until IS NULL OR s.lease_until<now()) AND EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=s.installation_id AND c.enabled) AND EXISTS(SELECT 1 FROM event_jobs j WHERE j.installation_id=s.installation_id AND j.status IN ('queued','retry','running') AND j.run_after<=now() AND (NOT $1 OR j.kind='current')) ORDER BY (SELECT min(j.priority) FROM event_jobs j WHERE j.installation_id=s.installation_id AND j.status IN ('queued','retry','running') AND j.run_after<=now()),s.updated_at FOR UPDATE OF s SKIP LOCKED LIMIT 1`, []any{false}, rel02ExplainClaim, "event_sources"},
			struct {
				name, sql string
				args      []any
				limit     time.Duration
				table     string
			}{"claim-enrichment", `SELECT o.installation_id,s.integration_id,o.object_kind,o.parent_type FROM event_enrichment_objects o JOIN event_sources s ON s.installation_id=o.installation_id WHERE ` + enrichmentClaimable + ` AND o.run_after<=now() AND (o.lease_until IS NULL OR o.lease_until<now()) AND s.state NOT IN ('reauth_required','disabled','paused','failed') AND EXISTS(SELECT 1 FROM event_consumers c WHERE c.installation_id=o.installation_id AND c.enabled) AND NOT EXISTS(SELECT 1 FROM event_jobs j JOIN event_sources src ON src.installation_id=j.installation_id WHERE j.installation_id=o.installation_id AND j.kind IN ('current','backfill') AND j.status='running' AND src.lease_until>now()) AND NOT EXISTS(SELECT 1 FROM event_enrichment_objects busy WHERE busy.installation_id=o.installation_id AND busy.lease_until>now()) ORDER BY o.run_after,o.object_kind,o.object_key FOR UPDATE OF o SKIP LOCKED LIMIT 1`, nil, rel02ExplainClaim, "event_enrichment_objects"},
		)
	}
	result := map[string]any{}
	rows := rel02Count(t, pool, `SELECT count(*) FROM crm_events`)
	for _, item := range queries {
		plan, parsed := rel02Explain(t, pool, item.sql, item.args)
		name := profile.ID + "-" + item.name
		rel02WriteText(t, filepath.Join(dir, "explain", name+".txt"), plan)
		parsed.Name = item.name
		result[item.name] = parsed
		if parsed.Execution > item.limit {
			report.fail(t, "%s EXPLAIN %s execution %s exceeds %s", profile.ID, item.name, parsed.Execution, item.limit)
		}
		if rows >= rel02SeqScanMinRows {
			for _, table := range parsed.SeqScans {
				if table == "crm_events" && item.name == "history-compact" {
					report.fail(t, "%s EXPLAIN history-compact seq scan on crm_events at %d rows; crm_events_time should serve the page", profile.ID, rows)
				}
			}
			if item.table == "crm_events" && item.name == "history-compact" && (parsed.SharedRead > rel02HighSharedRead || parsed.HeapFetches > rel02HighHeapFetches) {
				report.fail(t, "%s EXPLAIN %s high buffers read=%d heap=%d", profile.ID, item.name, parsed.SharedRead, parsed.HeapFetches)
			}
		}
	}
	return result
}

func rel02ProbeQuery(t *testing.T, report *rel02Report, svc *Service, gw *rel02Gateway, q serviceapi.Query, label string, p95, p99 time.Duration) map[string]any {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < rel02Warmup; i++ {
		if _, err := svc.Query(ctx, q); err != nil && serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
			t.Fatal(err)
		}
	}
	samples := make([]time.Duration, 0, rel02Samples)
	var last serviceapi.QueryResult
	var lastErr error
	before := gw.snapshot()
	for i := 0; i < rel02Samples; i++ {
		start := time.Now()
		last, lastErr = svc.Query(ctx, q)
		samples = append(samples, time.Since(start))
		if lastErr != nil && serviceapi.ErrorCode(lastErr) != serviceapi.ResourceExhausted {
			t.Fatal(lastErr)
		}
	}
	got := rel02Percentiles(samples)
	rel02CheckLatency(t, report, label, got, p95, p99)
	after := gw.snapshot()
	rel02ForbidEventsAPI(t, report, label, before, after)
	out := map[string]any{"p50": got.p50.String(), "p95": got.p95.String(), "p99": got.p99.String(), "max": got.max.String(), "samples": len(samples)}
	if lastErr != nil {
		out["last_error"] = lastErr.Error()
		out["last_code"] = string(serviceapi.ErrorCode(lastErr))
	} else {
		out["events"] = len(last.Events)
		out["summaries"] = len(last.Summaries)
		out["timeline"] = len(last.Timeline)
		encoded, _ := json.Marshal(last)
		out["json_bytes"] = len(encoded)
	}
	return out
}

func rel02ProbeCard(t *testing.T, report *rel02Report, svc *Service, gw *rel02Gateway, id, label string, p95, p99 time.Duration) map[string]any {
	t.Helper()
	ctx := context.Background()
	req := serviceapi.EventRequest{EventID: id}
	for i := 0; i < rel02Warmup; i++ {
		if _, err := svc.GetEvent(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	samples := make([]time.Duration, 0, rel02Samples)
	var last serviceapi.Event
	before := gw.snapshot()
	for i := 0; i < rel02Samples; i++ {
		start := time.Now()
		event, err := svc.GetEvent(ctx, req)
		samples = append(samples, time.Since(start))
		if err != nil {
			t.Fatal(err)
		}
		last = event
	}
	got := rel02Percentiles(samples)
	rel02CheckLatency(t, report, label, got, p95, p99)
	rel02ForbidEventsAPI(t, report, label, before, gw.snapshot())
	encoded, _ := json.Marshal(last)
	return map[string]any{"p50": got.p50.String(), "p95": got.p95.String(), "p99": got.p99.String(), "max": got.max.String(), "json_bytes": len(encoded), "enrichment": len(last.Enrichment)}
}

// Local product timings include JSON serialization; transport measurements use
// TestStage6TransportMeasure and the real generated RPC clients instead.
func rel02MeasureSizes(t *testing.T, report *rel02Report, svc *Service, gw *rel02Gateway, src rel02Source, q serviceapi.Query, cardID, label string) map[string]any {
	t.Helper()
	ctx := context.Background()
	act := activity.New(rel02Store{}, &testPolicy{principal: src.Principal}, svc, gw)
	before := gw.snapshot()
	measure := func(name string, p95, p99 time.Duration, call func() (any, error)) map[string]any {
		var samples []time.Duration
		var encoded []byte
		var lastErr error
		for i := -rel02Warmup; i < rel02Samples; i++ {
			start := time.Now()
			value, err := call()
			if err == nil {
				encoded, err = json.Marshal(value)
			}
			elapsed := time.Since(start)
			if err != nil && serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
				t.Fatal(err)
			}
			lastErr = err
			if i >= 0 {
				samples = append(samples, elapsed)
			}
		}
		lat := rel02Percentiles(samples)
		rel02CheckLatency(t, report, label+" "+name, lat, p95, p99)
		if len(encoded) > serviceapi.MaxResponseBytes {
			report.fail(t, "%s JSON exceeds response limit: %d", name, len(encoded))
		}
		out := map[string]any{"p95": lat.p95.String(), "p99": lat.p99.String(), "json_bytes": len(encoded), "samples": len(samples)}
		if lastErr != nil {
			out["error"] = string(serviceapi.ErrorCode(lastErr))
		}
		return out
	}
	out := map[string]any{
		"mode":       "local product plus JSON serialization",
		"panel":      measure("Panel", rel02PanelP95, rel02PanelP99, func() (any, error) { return act.Panel(ctx, q) }),
		"event_card": measure("EventCard", rel02EventCardP95, rel02EventCardP99, func() (any, error) { return act.EventCard(ctx, serviceapi.EventRequest{EventID: cardID}) }),
	}
	rel02ForbidEventsAPI(t, report, "local product "+label, before, gw.snapshot())
	return out
}

func rel02ProbeClaim(t *testing.T, report *rel02Report, store *Postgres, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	ctx := context.Background()
	samples := make([]time.Duration, 0, rel02Samples)
	for i := 0; i < rel02Samples; i++ {
		rel02ResetLeases(t, pool)
		start := time.Now()
		_, err := store.Claim(ctx)
		samples = append(samples, time.Since(start))
		if err != nil && err != ErrNoWork {
			t.Fatal(err)
		}
		if err == ErrNoWork {
			report.fail(t, "Claim returned no work on seeded jobs")
			break
		}
	}
	rel02ResetLeases(t, pool)
	got := rel02Percentiles(samples)
	rel02CheckLatency(t, report, "Claim", got, rel02ClaimP95, rel02ClaimP99)
	return map[string]any{"p50": got.p50.String(), "p95": got.p95.String(), "p99": got.p99.String(), "max": got.max.String()}
}

func rel02ProbeEnrichmentClaim(t *testing.T, report *rel02Report, store *Postgres, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	ctx := context.Background()
	samples := make([]time.Duration, 0, rel02Samples)
	var last EnrichmentClaim
	for i := 0; i < rel02Samples; i++ {
		rel02ResetLeases(t, pool)
		start := time.Now()
		c, err := store.ClaimEnrichment(ctx)
		samples = append(samples, time.Since(start))
		if err != nil && err != ErrNoWork {
			t.Fatal(err)
		}
		if err == ErrNoWork {
			report.fail(t, "ClaimEnrichment returned no work on seeded objects")
			break
		}
		last = c
	}
	rel02ResetLeases(t, pool)
	got := rel02Percentiles(samples)
	rel02CheckLatency(t, report, "ClaimEnrichment", got, rel02ClaimP95, rel02ClaimP99)
	return map[string]any{"p50": got.p50.String(), "p95": got.p95.String(), "p99": got.p99.String(), "objects": len(last.Objects), "kind": last.Kind}
}

func rel02QueueSnapshot(t *testing.T, store *Postgres, pool *pgxpool.Pool, gw *rel02Gateway, sources []rel02Source) map[string]any {
	t.Helper()
	ctx := context.Background()
	snap, err := store.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var claimable, refresh, pending, payload int64
	err = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE `+enrichmentClaimable+` AND o.run_after<=now() AND (o.lease_until IS NULL OR o.lease_until<now())),
		count(*) FILTER (WHERE o.state='ready' AND o.source<>'event_payload' AND o.run_after<=now()),
		count(*) FILTER (WHERE o.state IN ('pending','retry')),
		count(*) FILTER (WHERE o.source='event_payload')
		FROM event_enrichment_objects o`).Scan(&claimable, &refresh, &pending, &payload)
	if err != nil {
		t.Fatal(err)
	}
	lags := []int64{}
	for _, src := range sources {
		st, err := store.Status(ctx, src.Principal)
		if err != nil {
			t.Fatal(err)
		}
		lags = append(lags, st.LagSeconds)
	}
	share := 0.0
	if claimable > 0 {
		share = float64(refresh) / float64(claimable)
	}
	return map[string]any{
		"jobs": snap.States, "job_age_seconds": snap.AgeSeconds, "collector_lag_seconds": snap.LagSeconds,
		"enrichment": snap.EnrichmentStates, "enrichment_age_seconds": snap.EnrichmentAgeSeconds,
		"claimable": claimable, "refreshable_ready": refresh, "pending_or_retry": pending,
		"event_payload_objects": payload, "refresh_share_of_claimable": share,
		"status_lag_seconds": lags, "installations": len(sources), "amocrm": gw.snapshot(),
	}
}

type rel02Source struct {
	Principal serviceapi.Principal
	Users     []serviceapi.User
	From, To  int64
}

func rel02InsertSource(t *testing.T, pool *pgxpool.Pool, lagFrom time.Time) rel02Source {
	t.Helper()
	p := serviceapi.Principal{Scope: serviceapi.Scope{InstallationID: uuid.New(), IntegrationID: uuid.New()}, ActorID: 77, Consumer: "activity"}
	now := time.Now().UTC()
	_, err := pool.Exec(context.Background(), `INSERT INTO event_sources(installation_id,integration_id,state,continuous_from,continuous_to,retained_from,last_success_at,last_event_at) VALUES($1,$2,'idle',$3,$4,$3,$4,$4)`, p.InstallationID, p.IntegrationID, lagFrom.Add(-2*time.Hour), lagFrom)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), `INSERT INTO event_consumers(installation_id,consumer,enabled) VALUES($1,'activity',true)`, p.InstallationID); err != nil {
		t.Fatal(err)
	}
	_ = now
	return rel02Source{Principal: p}
}

func rel02Window(days int) (int64, int64) {
	to := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	return from.Unix(), to.Unix()
}

func rel02Users(n int) []serviceapi.User {
	users := make([]serviceapi.User, n)
	for i := 0; i < n; i++ {
		users[i] = serviceapi.User{ID: 71001 + int64(i), Name: fmt.Sprintf("user-%d", i+1), GroupID: 1, GroupName: "dept"}
	}
	return users
}

func rel02UserIDs(users []serviceapi.User) []int64 {
	ids := make([]int64, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	return ids
}

func rel02CopyEvents(t *testing.T, pool *pgxpool.Pool, src rel02Source, n int, payload []byte, unknown bool) {
	t.Helper()
	if n <= 0 {
		return
	}
	rows := make([][]any, 0, n)
	span := src.To - src.From
	if span <= 0 {
		span = 1
	}
	hash := []byte("stage6-rel02-measure-hash-bytes")
	users := src.Users
	for i := 0; i < n; i++ {
		kind := rel02Types[i%len(rel02Types)]
		createdBy := users[i%len(users)].ID
		if unknown && i%50 == 0 {
			createdBy = 0
		}
		if unknown && i%51 == 0 {
			createdBy = 71999
		}
		offset := int64(i) * span / int64(n)
		at := time.Unix(src.From+offset, 0).UTC()
		rows = append(rows, []any{
			src.Principal.InstallationID, fmt.Sprintf("e-%s-%08d", src.Principal.InstallationID.String()[:8], i),
			at, createdBy, kind.Type, int64(31000 + i%500), kind.EntityType, int64(0), payload, payload, hash,
		})
	}
	_, err := pool.CopyFrom(context.Background(), pgx.Identifier{"crm_events"},
		[]string{"installation_id", "event_id", "created_at", "created_by", "event_type", "entity_id", "entity_type", "linked_talk_contact_id", "value_before", "value_after", "content_hash"},
		pgx.CopyFromRows(rows))
	if err != nil {
		t.Fatal(err)
	}
}

func rel02SeedJobs(t *testing.T, pool *pgxpool.Pool, src rel02Source, idx int) {
	t.Helper()
	now := time.Now().UTC()
	from := time.Unix(src.From, 0).UTC()
	to := time.Unix(src.To, 0).UTC()
	current := uuid.New()
	_, err := pool.Exec(context.Background(), `INSERT INTO event_jobs(id,installation_id,kind,priority,window_from,window_to,target_to,status,run_after,created_at) VALUES($1,$2,'current',10,$3,$4,$5,'queued',$6,$7)`, current, src.Principal.InstallationID, from, minTime(from.Add(time.Hour), to), to, now.Add(-time.Minute), now.Add(-time.Duration(5+idx)*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 3; j++ {
		bf := uuid.New()
		wFrom := from.Add(time.Duration(j) * time.Hour)
		_, err = pool.Exec(context.Background(), `INSERT INTO event_jobs(id,installation_id,kind,priority,window_from,window_to,target_to,status,run_after,created_at) VALUES($1,$2,'backfill',100,$3,$4,$5,'queued',$6,$7)`, bf, src.Principal.InstallationID, wFrom, minTime(wFrom.Add(time.Hour), to), to, now.Add(-time.Duration(j)*time.Minute), now.Add(-time.Duration(20+idx*3+j)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func rel02SeedEnrichment(t *testing.T, pool *pgxpool.Pool, src rel02Source, events int, heavy bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	n := 40
	if heavy {
		n = 120
	}
	if n > events {
		n = events
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("obj-%d", i)
		kind := serviceapi.ObjectNote
		state, source, reason := serviceapi.EnrichmentPending, "", serviceapi.ReasonNotLoaded
		runAfter := now.Add(-time.Minute)
		switch i % 5 {
		case 0:
			state, source, reason = serviceapi.EnrichmentReady, serviceapi.SourceNotesAPI, ""
			runAfter = now.Add(-time.Minute)
		case 1:
			state, source, reason = serviceapi.EnrichmentReady, serviceapi.SourceEventPayload, ""
			runAfter = now.Add(time.Hour)
		case 2:
			state, source, reason = serviceapi.EnrichmentRetry, "", serviceapi.ReasonTemporary
		case 3:
			state, source, reason = serviceapi.EnrichmentPending, "", serviceapi.ReasonNotLoaded
		default:
			state, source, reason = serviceapi.EnrichmentUnavailable, serviceapi.SourceNotesAPI, serviceapi.ReasonNotFound
			runAfter = now.Add(-time.Minute)
		}
		_, err := pool.Exec(ctx, `INSERT INTO event_enrichment_objects(installation_id,object_kind,object_key,parent_type,parent_id,object_id,state,reason_code,source,payload,run_after,updated_at) VALUES($1,$2,$3,'leads',$4,$4,$5,$6,$7,'{"ok":true}',$8,$9)`, src.Principal.InstallationID, kind, key, int64(31000+i), state, reason, source, runAfter, now.Add(-time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		eventID := fmt.Sprintf("e-%s-%08d", src.Principal.InstallationID.String()[:8], i)
		_, err = pool.Exec(ctx, `INSERT INTO event_enrichment_links(installation_id,event_id,object_kind,object_key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, src.Principal.InstallationID, eventID, kind, key)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func rel02ResetLeases(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE event_sources SET lease_until=NULL,state='idle'; UPDATE event_jobs SET status='queued',lease_token=0 WHERE status IN ('queued','running','retry'); UPDATE event_enrichment_objects SET lease_until=NULL`); err != nil {
		t.Fatal(err)
	}
}

func rel02AnyEventID(t *testing.T, pool *pgxpool.Pool, installation uuid.UUID) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT event_id FROM crm_events WHERE installation_id=$1 ORDER BY created_at,event_id LIMIT 1`, installation).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func rel02Count(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type rel02Plan struct {
	Name        string        `json:"name"`
	Execution   time.Duration `json:"execution"`
	SharedHit   int           `json:"shared_hit"`
	SharedRead  int           `json:"shared_read"`
	HeapFetches int           `json:"heap_fetches"`
	SeqScans    []string      `json:"seq_scans"`
	Indexes     []string      `json:"indexes"`
	ExecutionMS float64       `json:"execution_ms"`
}

func rel02Explain(t *testing.T, pool *pgxpool.Pool, sql string, args []any) (string, rel02Plan) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	var b strings.Builder
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	plan := b.String()
	parsed := rel02Plan{}
	if m := rel02ExecRe.FindStringSubmatch(plan); len(m) == 2 {
		var ms float64
		fmt.Sscanf(m[1], "%f", &ms)
		parsed.ExecutionMS = ms
		parsed.Execution = time.Duration(ms * float64(time.Millisecond))
	}
	if m := rel02HitRe.FindStringSubmatch(plan); len(m) == 2 {
		fmt.Sscanf(m[1], "%d", &parsed.SharedHit)
	}
	if m := rel02ReadRe.FindStringSubmatch(plan); len(m) == 2 {
		fmt.Sscanf(m[1], "%d", &parsed.SharedRead)
	}
	if m := rel02HeapRe.FindStringSubmatch(plan); len(m) == 2 {
		fmt.Sscanf(m[1], "%d", &parsed.HeapFetches)
	}
	for _, m := range rel02SeqRe.FindAllStringSubmatch(plan, -1) {
		parsed.SeqScans = append(parsed.SeqScans, m[1])
	}
	for _, m := range rel02IndexRe.FindAllStringSubmatch(plan, -1) {
		parsed.Indexes = append(parsed.Indexes, m[1])
	}
	return plan, parsed
}

type rel02Lat struct{ p50, p95, p99, max time.Duration }

func rel02Percentiles(samples []time.Duration) rel02Lat {
	if len(samples) == 0 {
		return rel02Lat{}
	}
	sorted := append([]time.Duration{}, samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) time.Duration {
		idx := int(math.Ceil(p*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}
	return rel02Lat{p50: pick(0.50), p95: pick(0.95), p99: pick(0.99), max: sorted[len(sorted)-1]}
}

func rel02CheckLatency(t *testing.T, report *rel02Report, label string, got rel02Lat, p95, p99 time.Duration) {
	t.Helper()
	if got.p95 > p95 {
		report.fail(t, "%s P95 %s exceeds %s", label, got.p95, p95)
	}
	if got.p99 > p99 {
		report.fail(t, "%s P99 %s exceeds %s", label, got.p99, p99)
	}
}

type rel02Calls struct {
	Events, Users, Notes, Tasks, Pipelines, Fields, Entities int
}

type rel02Gateway struct {
	mu    sync.Mutex
	users []serviceapi.User
	rel02Calls
}

func (g *rel02Gateway) snapshot() rel02Calls {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rel02Calls
}

func (g *rel02Gateway) Events(context.Context, serviceapi.EventPageRequest) (serviceapi.EventPage, error) {
	g.mu.Lock()
	g.rel02Calls.Events++
	g.mu.Unlock()
	return serviceapi.EventPage{}, serviceapi.Fail(serviceapi.Unavailable, "amoCRM events must not be called on the read path")
}
func (g *rel02Gateway) Users(_ context.Context, r serviceapi.UsersRequest) (serviceapi.Directory, error) {
	g.mu.Lock()
	g.rel02Calls.Users++
	users := g.users
	g.mu.Unlock()
	if len(r.UserIDs) == 0 {
		return serviceapi.Directory{Users: users, Timezone: "UTC"}, nil
	}
	allow := map[int64]bool{}
	for _, id := range r.UserIDs {
		allow[id] = true
	}
	var filtered []serviceapi.User
	for _, u := range users {
		if allow[u.ID] {
			filtered = append(filtered, u)
		}
	}
	return serviceapi.Directory{Users: filtered, Timezone: "UTC"}, nil
}
func (g *rel02Gateway) Notes(context.Context, serviceapi.NotesRequest) (serviceapi.NotePage, error) {
	g.mu.Lock()
	g.rel02Calls.Notes++
	g.mu.Unlock()
	return serviceapi.NotePage{}, serviceapi.Fail(serviceapi.Unavailable, "notes")
}
func (g *rel02Gateway) Tasks(context.Context, serviceapi.TasksRequest) (serviceapi.TaskPage, error) {
	g.mu.Lock()
	g.rel02Calls.Tasks++
	g.mu.Unlock()
	return serviceapi.TaskPage{}, serviceapi.Fail(serviceapi.Unavailable, "tasks")
}
func (g *rel02Gateway) Pipelines(context.Context, serviceapi.CatalogRequest) (serviceapi.PipelineCatalog, error) {
	g.mu.Lock()
	g.rel02Calls.Pipelines++
	g.mu.Unlock()
	return serviceapi.PipelineCatalog{}, serviceapi.Fail(serviceapi.Unavailable, "pipelines")
}
func (g *rel02Gateway) CustomFields(context.Context, serviceapi.CustomFieldsRequest) (serviceapi.CustomFieldCatalog, error) {
	g.mu.Lock()
	g.rel02Calls.Fields++
	g.mu.Unlock()
	return serviceapi.CustomFieldCatalog{}, serviceapi.Fail(serviceapi.Unavailable, "fields")
}
func (g *rel02Gateway) Entities(context.Context, serviceapi.EntitiesRequest) (serviceapi.EntityCatalog, error) {
	g.mu.Lock()
	g.rel02Calls.Entities++
	g.mu.Unlock()
	return serviceapi.EntityCatalog{}, serviceapi.Fail(serviceapi.Unavailable, "entities")
}

func rel02ForbidEventsAPI(t *testing.T, report *rel02Report, label string, before, after rel02Calls) {
	t.Helper()
	if after.Events != before.Events || after.Notes != before.Notes || after.Tasks != before.Tasks || after.Pipelines != before.Pipelines || after.Fields != before.Fields || after.Entities != before.Entities {
		report.fail(t, "%s called amoCRM collection APIs: %+v -> %+v", label, before, after)
	}
}

type rel02Store struct{}

func (rel02Store) Settings(context.Context, serviceapi.Scope) (serviceapi.Settings, error) {
	return serviceapi.DefaultSettings(), nil
}
func (rel02Store) Configure(context.Context, serviceapi.Principal, serviceapi.SettingsCommand) (serviceapi.Operation, error) {
	return serviceapi.Operation{}, nil
}
func (rel02Store) Operation(context.Context, serviceapi.Principal, string) (serviceapi.Operation, error) {
	return serviceapi.Operation{}, nil
}
func (rel02Store) ResolveShare(context.Context, []byte) (serviceapi.ShareLookup, error) {
	return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
}
func (rel02Store) CreatePanel(context.Context, serviceapi.Principal, serviceapi.PanelCommand, serviceapi.ManagedPanel, []byte) (serviceapi.ManagedPanel, error) {
	return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Unavailable, "panels unavailable")
}
func (rel02Store) ListPanels(context.Context, serviceapi.Scope) ([]serviceapi.ManagedPanel, error) {
	return nil, serviceapi.Fail(serviceapi.Unavailable, "panels unavailable")
}
func (rel02Store) GetPanel(context.Context, serviceapi.Scope, uuid.UUID) (serviceapi.ManagedPanel, error) {
	return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
}
func (rel02Store) PatchPanel(context.Context, serviceapi.Principal, serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
}
func (rel02Store) RotateShareLink(context.Context, serviceapi.Principal, serviceapi.PanelCommand, []byte, string) (serviceapi.ManagedPanel, error) {
	return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
}

type rel02Report struct {
	Thresholds map[string]string `json:"thresholds"`
	Started    time.Time         `json:"started"`
	Finished   time.Time         `json:"finished"`
	Elapsed    string            `json:"elapsed"`
	Failures   []string          `json:"failures"`
}

func (r *rel02Report) fail(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	r.Failures = append(r.Failures, msg)
	t.Errorf("%s", msg)
}

func rel02ThresholdMap() map[string]string {
	return map[string]string{
		"query_p95": rel02QueryP95.String(), "query_p99": rel02QueryP99.String(),
		"card_p95": rel02CardP95.String(), "card_p99": rel02CardP99.String(),
		"claim_p95": rel02ClaimP95.String(), "claim_p99": rel02ClaimP99.String(),
		"panel_p95": rel02PanelP95.String(), "eventcard_p95": rel02EventCardP95.String(),
		"explain_history": rel02ExplainHistory.String(), "explain_aggregate": rel02ExplainAggregate.String(),
		"max_response_bytes": fmt.Sprintf("%d", serviceapi.MaxResponseBytes),
		"max_rpc_bytes":      fmt.Sprintf("%d", servicerpc.MaxMessageSize),
	}
}

func rel02Stand(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var version, shared, work, cache, maxConn string
	var dbBytes int64
	err := pool.QueryRow(context.Background(), `SELECT version(),current_setting('shared_buffers'),current_setting('work_mem'),current_setting('effective_cache_size'),current_setting('max_connections'),pg_database_size(current_database())`).Scan(&version, &shared, &work, &cache, &maxConn, &dbBytes)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"docker_ncpu": 2, "docker_mem_bytes": 4094447616, "docker_arch": "aarch64",
		"postgres_version": version, "shared_buffers": shared, "work_mem": work,
		"effective_cache_size": cache, "max_connections": maxConn, "database_bytes": dbBytes,
		"compose_postgres_mem_limit": "512m", "database": "events_components_test",
	}
}

func rel02ArtifactDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("STAGE6_REL02_ARTIFACT_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "docs", "verification", "stage6-2026-09-08", "rel-02")
	}
	for _, sub := range []string{"", "explain", "profiles"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func rel02WriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rel02WriteText(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
