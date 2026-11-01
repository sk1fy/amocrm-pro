// Package requesttiming records bounded, inclusive request stage totals.
package requesttiming

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/buildinfo"
)

type Service uint8

const (
	Core Service = iota
	Activity
	Events
	Gateway
	serviceCount
)

var serviceNames = [...]string{"core", "activity", "events", "gateway"}

func ServiceForIdentity(s string) (Service, bool) {
	for i, n := range []string{"core", "activity", "crm-events", "gateway"} {
		if s == n {
			return Service(i), true
		}
	}
	return 0, false
}

type Stage uint8

const (
	Handler Stage = iota
	RPCClient
	RPCServer
	EmployeeVerify
	SQL
	Acquire
	Limiter
	AmoHTTP
	TokenLookup
	TokenRefresh
	QueueLoad
	QueueRefresh
	QueueWait
	QueueInitial
	QueueFinal
	stageCount
)

var stageNames = [...]string{"handler", "rpc_client", "rpc_server", "employee_verify", "sql", "acquire", "limiter", "amo_http", "token_lookup", "token_refresh", "queue_load", "queue_refresh", "queue_wait", "queue_initial", "queue_final"}

type Method uint8

const (
	Local Method = iota
	Issue
	Validate
	OwnTasks
	OwnTask
	OwnLead
	CompleteTask
	methodCount
)

var methodNames = [...]string{"local", "issue", "validate", "tasks", "task", "lead", "complete"}

type Outcome uint8

const (
	OK Outcome = iota
	Failed
	Canceled
	Deadline
	Oversize
	Pending
	MissingRow
	MissingPayload
	Expired
	Fresh
	Hit
	Refreshed
	Stale
	Backoff
	WaitTimeout
	outcomeCount
)

var outcomeNames = [...]string{"ok", "error", "canceled", "deadline", "oversize", "pending", "missing_row", "missing_payload", "expired", "fresh", "hit", "refreshed", "stale", "backoff", "wait_timeout"}

const MaxCells = 64
const MaxHeaderBytes = 2800

type key struct {
	Service Service
	Stage   Stage
	Method  Method
	Outcome Outcome
}
type Cell struct {
	Service Service `json:"s"`
	Stage   Stage   `json:"g"`
	Method  Method  `json:"m"`
	Outcome Outcome `json:"o"`
	Count   uint32  `json:"n"`
	Micros  int64   `json:"u"`
}
type Limits struct {
	PairRPS        float64 `json:"p"`
	AccountRPS     float64 `json:"a"`
	PairBurst      int     `json:"b"`
	AccountBurst   int     `json:"c"`
	MaxWaitMS      int64   `json:"w"`
	PairWaiters    int     `json:"x"`
	AccountWaiters int     `json:"y"`
	ProcessWaiters int     `json:"z"`
	Replicas       int     `json:"r"`
}
type Config struct {
	Present  bool   `json:"p,omitempty"`
	Multiple bool   `json:"m,omitempty"`
	Limits   Limits `json:"l"`
}

// Summary is a value snapshot. Durations overlap and must not be added as latency.
type Summary struct {
	Cells      [MaxCells]Cell
	Count      int
	Incomplete bool
	Revisions  [serviceCount]string
	Configs    [serviceCount]Config
}
type active struct {
	stage  Stage
	method Method
	count  uint32
	starts time.Duration
}
type recorder struct {
	mu          sync.Mutex
	id          uuid.UUID
	service     Service
	method      Method
	start       time.Time
	summary     Summary
	active      [MaxCells]active
	activeCount int
	sealed      bool
}
type contextKey struct{}

func Start(ctx context.Context, id uuid.UUID, service Service, method Method) context.Context {
	if id == uuid.Nil || service >= serviceCount || method >= methodCount {
		return ctx
	}
	r := &recorder{id: id, service: service, method: method, start: time.Now()}
	r.summary.Revisions[service] = safeRevision(buildinfo.SourceRevision())
	return context.WithValue(ctx, contextKey{}, r)
}
func get(ctx context.Context) *recorder { r, _ := ctx.Value(contextKey{}).(*recorder); return r }
func Enabled(ctx context.Context) bool  { return get(ctx) != nil }
func RequestID(ctx context.Context) uuid.UUID {
	if r := get(ctx); r != nil {
		return r.id
	}
	return uuid.Nil
}
func Measure(ctx context.Context, stage Stage) func(error) { return MeasureMethod(ctx, stage, Local) }
func MeasureMethod(ctx context.Context, stage Stage, method Method) func(error) {
	r := get(ctx)
	if r == nil {
		return func(error) {}
	}
	if method == Local {
		method = r.method
	}
	start := time.Now()
	r.mu.Lock()
	if r.sealed {
		r.mu.Unlock()
		return func(error) {}
	}
	slot := -1
	for i := 0; i < r.activeCount; i++ {
		if r.active[i].stage == stage && r.active[i].method == method {
			slot = i
			break
		}
	}
	if slot < 0 && r.activeCount < MaxCells {
		slot = r.activeCount
		r.activeCount++
		r.active[slot] = active{stage: stage, method: method}
	}
	if slot < 0 {
		r.summary.Incomplete = true
		r.mu.Unlock()
		return func(error) {}
	}
	r.active[slot].count++
	r.active[slot].starts += start.Sub(r.start)
	r.mu.Unlock()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.sealed {
				return
			}
			r.active[slot].count--
			r.active[slot].starts -= start.Sub(r.start)
			add(&r.summary, Cell{r.service, stage, method, outcome(err), 1, time.Since(start).Microseconds()})
		})
	}
}
func outcome(err error) Outcome {
	switch {
	case err == nil:
		return OK
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return Deadline
	default:
		return Failed
	}
}
func Observe(ctx context.Context, stage Stage, result Outcome) {
	if r := get(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.sealed {
			add(&r.summary, Cell{r.service, stage, r.method, result, 1, 0})
		}
	}
}
func ObserveLimits(ctx context.Context, limits Limits) {
	if r := get(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sealed {
			return
		}
		c := &r.summary.Configs[r.service]
		if c.Present && c.Limits != limits {
			c.Multiple = true
		}
		c.Present = true
		c.Limits = limits
	}
}
func Incomplete(ctx context.Context) {
	if r := get(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.sealed {
			r.summary.Incomplete = true
		}
	}
}
func Snapshot(ctx context.Context) Summary {
	r := get(ctx)
	if r == nil {
		return Summary{Incomplete: true}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sealed {
		elapsed := time.Since(r.start)
		for i := 0; i < r.activeCount; i++ {
			a := r.active[i]
			if a.count > 0 {
				add(&r.summary, Cell{r.service, a.stage, a.method, Pending, a.count, (time.Duration(a.count)*elapsed - a.starts).Microseconds()})
				r.summary.Incomplete = true
			}
		}
		r.sealed = true
	}
	return r.summary
}
func Merge(ctx context.Context, s Summary) {
	if r := get(ctx); r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sealed {
			return
		}
		r.summary.Incomplete = r.summary.Incomplete || s.Incomplete
		for i := 0; i < s.Count; i++ {
			add(&r.summary, s.Cells[i])
		}
		for i, rev := range s.Revisions {
			if rev != "" {
				if r.summary.Revisions[i] != "" && r.summary.Revisions[i] != rev {
					r.summary.Revisions[i] = "multiple"
				} else {
					r.summary.Revisions[i] = rev
				}
			}
		}
		for i, c := range s.Configs {
			if c.Present {
				old := r.summary.Configs[i]
				if old.Present && old.Limits != c.Limits {
					c.Multiple = true
				}
				c.Multiple = c.Multiple || old.Multiple
				r.summary.Configs[i] = c
			}
		}
	}
}
func add(s *Summary, c Cell) {
	for i := 0; i < s.Count; i++ {
		old := &s.Cells[i]
		if old.Service == c.Service && old.Stage == c.Stage && old.Method == c.Method && old.Outcome == c.Outcome {
			if uint64(old.Count)+uint64(c.Count) > 1_000_000 || old.Micros+c.Micros > int64(24*time.Hour/time.Microsecond) {
				s.Incomplete = true
				return
			}
			old.Count += c.Count
			old.Micros += c.Micros
			return
		}
	}
	if s.Count == MaxCells {
		s.Incomplete = true
		return
	}
	s.Cells[s.Count] = c
	s.Count++
}

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{7,64}(-dirty(-[a-f0-9]{8})?)?$`)

func safeRevision(s string) string {
	if revisionPattern.MatchString(s) {
		return s
	}
	return "unknown"
}
func (s Summary) ServerTiming() string {
	entries := make([]string, 0, s.Count+8)
	for i := 0; i < s.Count; i++ {
		c := s.Cells[i]
		entries = append(entries, fmt.Sprintf(`%s_%s_%s_%s;dur=%.3f;desc="n=%d"`, serviceNames[c.Service], methodNames[c.Method], stageNames[c.Stage], outcomeNames[c.Outcome], float64(c.Micros)/1000, c.Count))
	}
	for i, rev := range s.Revisions {
		if rev != "" {
			entries = append(entries, fmt.Sprintf(`%s_revision;desc="%s"`, serviceNames[i], rev))
		}
	}
	for i, c := range s.Configs {
		if c.Present {
			l := c.Limits
			entries = append(entries, fmt.Sprintf(`%s_limits;desc="pair=%g;account=%g;pb=%d;ab=%d;wait_ms=%d;pw=%d;aw=%d;tw=%d;replicas=%d;multiple=%t"`, serviceNames[i], l.PairRPS, l.AccountRPS, l.PairBurst, l.AccountBurst, l.MaxWaitMS, l.PairWaiters, l.AccountWaiters, l.ProcessWaiters, l.Replicas, c.Multiple))
		}
	}
	truncated := false
	for {
		complete := !s.Incomplete && !truncated
		controls := fmt.Sprintf(`diag;desc="v=1;n=%d;complete=%t;truncated=%t;inclusive=1"`, len(entries), complete, truncated)
		ending := fmt.Sprintf(`diag_end;desc="n=%d"`, len(entries))
		all := append([]string{controls}, entries...)
		all = append(all, ending)
		header := strings.Join(all, ", ")
		if len(header) <= MaxHeaderBytes {
			return header
		}
		entries = entries[:len(entries)-1]
		truncated = true
	}
}
