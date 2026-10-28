package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// The old PHP proxy kept a queue for 30 seconds; tabs poll it every minute.
const (
	TaskQueueTTL          = 30 * time.Second
	taskQueueLease        = 20 * time.Second
	taskQueueLoadTimeout  = 15 * time.Second
	taskQueueWait         = 8 * time.Second
	taskQueuePoll         = 200 * time.Millisecond
	taskQueuePermanentGap = time.Minute
	taskQueueMaxBackoff   = time.Minute
	// The completion lease outlives its holder's bounded amoCRM work
	// (re-read and PATCH, then a reconciling read; HTTP timeout is 15 s).
	taskCompletionLease     = 45 * time.Second
	taskCompletionWork      = 25 * time.Second
	taskCompletionReconcile = 10 * time.Second
	taskCompletionWait      = 5 * time.Second
)

type storedTask struct {
	ID           int64  `json:"id"`
	EntityID     int64  `json:"entity_id"`
	EntityType   string `json:"entity_type"`
	EntityName   string `json:"entity_name,omitempty"`
	Text         string `json:"text"`
	CompleteTill int64  `json:"complete_till"`
	TaskTypeID   int64  `json:"task_type_id"`
	UpdatedAt    int64  `json:"updated_at"`
}

type queueSnapshot struct {
	Tasks      []storedTask
	Complete   bool
	FetchedAt  time.Time
	FreshUntil time.Time
	Stale      bool
	ErrorCode  string
	RetryAfter time.Duration
}

type queueFetch func(context.Context) ([]storedTask, bool, error)

// TaskQueueStore coalesces queue loads across tabs and replicas. Freshness,
// leases and backoff compare against PostgreSQL's clock, never a replica's.
type TaskQueueStore struct {
	pool         *pgxpool.Pool
	ttl          time.Duration
	wait         time.Duration
	poll         time.Duration
	completeWait time.Duration
}

func NewTaskQueueStore(pool *pgxpool.Pool) *TaskQueueStore {
	return &TaskQueueStore{pool: pool, ttl: TaskQueueTTL, wait: taskQueueWait, poll: taskQueuePoll, completeWait: taskCompletionWait}
}

func (s *Service) WithTaskQueues(store *TaskQueueStore) *Service {
	s.taskQueues = store
	return s
}

type queueRow struct {
	payload    []byte
	complete   bool
	fetchedAt  *time.Time
	freshUntil time.Time
	retryAt    time.Time
	errorCode  string
	now        time.Time
}

func (r queueRow) snapshot(stale bool) (queueSnapshot, error) {
	out := queueSnapshot{Complete: r.complete, FreshUntil: r.freshUntil, Stale: stale, ErrorCode: r.errorCode}
	if r.fetchedAt != nil {
		out.FetchedAt = *r.fetchedAt
	}
	if err := json.Unmarshal(r.payload, &out.Tasks); err != nil {
		return queueSnapshot{}, serviceapi.Fail(serviceapi.Internal, "invalid cached task queue")
	}
	if r.now.Before(r.retryAt) {
		out.RetryAfter = r.retryAt.Sub(r.now)
	}
	return out, nil
}

// Load returns a fresh shared snapshot, refreshing it under a lease when
// needed. While another holder refreshes or amoCRM backs off, an existing
// snapshot is served as stale instead of starting another upstream read.
func (s *TaskQueueStore) Load(ctx context.Context, scope serviceapi.Scope, userID int64, fetch queueFetch) (queueSnapshot, error) {
	deadline := time.Now().Add(s.wait)
	for {
		row, found, err := s.read(ctx, scope, userID)
		if err != nil {
			return queueSnapshot{}, err
		}
		if found && row.payload != nil && row.now.Before(row.freshUntil) {
			return row.snapshot(false)
		}
		if found && row.now.Before(row.retryAt) {
			if row.payload != nil {
				return row.snapshot(true)
			}
			return queueSnapshot{}, backoffError(row.errorCode, row.retryAt.Sub(row.now))
		}
		token, version, acquired, err := s.acquire(ctx, scope, userID)
		if err != nil {
			return queueSnapshot{}, err
		}
		if acquired {
			return s.refresh(ctx, scope, userID, token, version, row, found, fetch)
		}
		if found && row.payload != nil {
			return row.snapshot(true)
		}
		if !time.Now().Before(deadline) {
			return queueSnapshot{}, &serviceapi.Error{Code: serviceapi.Unavailable, Message: "task queue refresh is in progress", RetryAfter: time.Second}
		}
		timer := time.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return queueSnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *TaskQueueStore) read(ctx context.Context, scope serviceapi.Scope, userID int64) (queueRow, bool, error) {
	var row queueRow
	err := s.pool.QueryRow(ctx, `SELECT payload,complete,fetched_at,fresh_until,retry_at,error_code,now() FROM activity_task_queues WHERE installation_id=$1 AND user_id=$2 AND integration_id=$3`, scope.InstallationID, userID, scope.IntegrationID).Scan(&row.payload, &row.complete, &row.fetchedAt, &row.freshUntil, &row.retryAt, &row.errorCode, &row.now)
	if errors.Is(err, pgx.ErrNoRows) {
		return queueRow{}, false, nil
	}
	if err != nil {
		return queueRow{}, false, serviceapi.Fail(serviceapi.Unavailable, "task queue store is unavailable")
	}
	return row, true, nil
}

// acquire takes the refresh lease only when no snapshot is fresh, no backoff
// is active and the previous lease expired. The token fences late writers.
func (s *TaskQueueStore) acquire(ctx context.Context, scope serviceapi.Scope, userID int64) (int64, int64, bool, error) {
	var token, version int64
	err := s.pool.QueryRow(ctx, `INSERT INTO activity_task_queues(installation_id,user_id,integration_id,lease_token,lease_until)
 VALUES($1,$2,$3,1,now()+make_interval(secs=>$4)) ON CONFLICT(installation_id,user_id) DO UPDATE SET
 lease_token=activity_task_queues.lease_token+1, lease_until=excluded.lease_until, accessed_at=now()
 WHERE activity_task_queues.integration_id=excluded.integration_id
 AND (activity_task_queues.lease_until IS NULL OR activity_task_queues.lease_until<=now())
 AND activity_task_queues.retry_at<=now()
 AND (activity_task_queues.payload IS NULL OR activity_task_queues.fresh_until<=now())
 RETURNING lease_token,version`, scope.InstallationID, userID, scope.IntegrationID, taskQueueLease.Seconds()).Scan(&token, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, serviceapi.Fail(serviceapi.Unavailable, "task queue store is unavailable")
	}
	return token, version, true, nil
}

func (s *TaskQueueStore) refresh(ctx context.Context, scope serviceapi.Scope, userID, token, version int64, previous queueRow, found bool, fetch queueFetch) (queueSnapshot, error) {
	// A closed tab must not waste a load other tabs are waiting for.
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskQueueLoadTimeout)
	defer cancel()
	tasks, complete, fetchErr := fetch(loadCtx)
	if fetchErr != nil {
		retry, err := s.recordFailure(loadCtx, scope, userID, token, fetchErr)
		if err != nil {
			return queueSnapshot{}, err
		}
		if found && previous.payload != nil {
			snap, err := previous.snapshot(true)
			snap.ErrorCode, snap.RetryAfter = string(serviceapi.ErrorCode(fetchErr)), retry
			return snap, err
		}
		return queueSnapshot{}, backoffError(string(serviceapi.ErrorCode(fetchErr)), retry)
	}
	payload, err := json.Marshal(tasks)
	if err != nil {
		return queueSnapshot{}, serviceapi.Fail(serviceapi.Internal, "encode task queue")
	}
	out := queueSnapshot{Tasks: tasks, Complete: complete}
	// A completion during this load bumps the version: keep the rows for
	// display but leave them due and stale, so the next read loads again.
	var current bool
	err = s.pool.QueryRow(loadCtx, `UPDATE activity_task_queues SET payload=$4, complete=$5, fetched_at=now(),
 fresh_until=CASE WHEN version=$6 THEN now()+make_interval(secs=>$7) ELSE now() END,
 lease_until=NULL, failures=0, retry_at=now(), error_code='', accessed_at=now()
 WHERE installation_id=$1 AND user_id=$2 AND lease_token=$3 RETURNING fetched_at,fresh_until,version=$6`, scope.InstallationID, userID, token, payload, complete, version, s.ttl.Seconds()).Scan(&out.FetchedAt, &out.FreshUntil, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		// The lease expired and a newer holder owns the row; this read is
		// still a valid observation for this caller only.
		out.FetchedAt, out.FreshUntil = time.Now(), time.Now()
		return out, nil
	}
	if err != nil {
		return queueSnapshot{}, serviceapi.Fail(serviceapi.Unavailable, "task queue store is unavailable")
	}
	out.Stale = !current
	s.sweep(loadCtx)
	return out, nil
}

func (s *TaskQueueStore) recordFailure(ctx context.Context, scope serviceapi.Scope, userID, token int64, cause error) (time.Duration, error) {
	code := serviceapi.ErrorCode(cause)
	var failures int
	if err := s.pool.QueryRow(ctx, `SELECT failures FROM activity_task_queues WHERE installation_id=$1 AND user_id=$2`, scope.InstallationID, userID).Scan(&failures); err != nil {
		return 0, serviceapi.Fail(serviceapi.Unavailable, "task queue store is unavailable")
	}
	retry := failureBackoff(code, failures+1, cause)
	_, err := s.pool.Exec(ctx, `UPDATE activity_task_queues SET lease_until=NULL, failures=failures+1,
 retry_at=now()+make_interval(secs=>$4), error_code=$5 WHERE installation_id=$1 AND user_id=$2 AND lease_token=$3`, scope.InstallationID, userID, token, retry.Seconds(), string(code))
	if err != nil {
		return 0, serviceapi.Fail(serviceapi.Unavailable, "task queue store is unavailable")
	}
	return retry, nil
}

// failureBackoff honours amoCRM's Retry-After and grows with jitter, so tabs
// of many employees do not retry in lockstep. Authorization and permission
// failures cannot heal by retrying quickly.
func failureBackoff(code serviceapi.Code, failures int, cause error) time.Duration {
	switch code {
	case serviceapi.ReauthRequired, serviceapi.PermissionDenied, serviceapi.InvalidArgument, serviceapi.NotFound:
		return taskQueuePermanentGap
	}
	delay := time.Second << min(failures-1, 6)
	delay = min(delay, taskQueueMaxBackoff)
	delay += time.Duration(rand.Int64N(int64(delay)/2 + 1))
	var upstream *serviceapi.Error
	if errors.As(cause, &upstream) && upstream.RetryAfter > delay {
		delay = upstream.RetryAfter
	}
	return delay
}

func backoffError(code string, retry time.Duration) error {
	c := serviceapi.Code(code)
	switch c {
	case serviceapi.ReauthRequired, serviceapi.PermissionDenied, serviceapi.ResourceExhausted, serviceapi.DeadlineExceeded, serviceapi.NotFound, serviceapi.InvalidArgument:
	default:
		c = serviceapi.Unavailable
	}
	return &serviceapi.Error{Code: c, Message: "task queue is backing off after an amoCRM failure", RetryAfter: retry}
}

// RemoveTask drops a task this caller completed without another amoCRM read.
// The version bump makes an overlapping load, the first one included,
// publish as already due.
func (s *TaskQueueStore) RemoveTask(ctx context.Context, scope serviceapi.Scope, userID, taskID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE activity_task_queues SET version=version+1,
 payload=CASE WHEN payload IS NULL THEN NULL ELSE (SELECT COALESCE(jsonb_agg(t.value ORDER BY t.ord),'[]'::jsonb) FROM jsonb_array_elements(payload) WITH ORDINALITY AS t(value,ord) WHERE (t.value->>'id')::bigint<>$3) END
 WHERE installation_id=$1 AND user_id=$2`, scope.InstallationID, userID, taskID)
	return err
}

// Invalidate marks the snapshot due after a write whose outcome is unknown.
func (s *TaskQueueStore) Invalidate(ctx context.Context, scope serviceapi.Scope, userID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE activity_task_queues SET version=version+1, fresh_until=LEAST(fresh_until,now()) WHERE installation_id=$1 AND user_id=$2`, scope.InstallationID, userID)
	return err
}

var errCompletionBusy = errors.New("task completion is in progress")

// AcquireCompletion serializes one task's completion across tabs and
// replicas. Only this row is held across the amoCRM call, never a lock; the
// lease expires by PostgreSQL's clock if its holder dies.
func (s *TaskQueueStore) AcquireCompletion(ctx context.Context, installation uuid.UUID, taskID int64) (uuid.UUID, error) {
	token := uuid.New()
	deadline := time.Now().Add(s.completeWait)
	for {
		tag, err := s.pool.Exec(ctx, `INSERT INTO activity_task_completions(installation_id,task_id,lease_token,lease_until)
 VALUES($1,$2,$3,now()+make_interval(secs=>$4)) ON CONFLICT(installation_id,task_id) DO UPDATE SET
 lease_token=excluded.lease_token, lease_until=excluded.lease_until WHERE activity_task_completions.lease_until<=now()`, installation, taskID, token, taskCompletionLease.Seconds())
		if err != nil {
			return uuid.Nil, serviceapi.Fail(serviceapi.Unavailable, "task completion store is unavailable")
		}
		if tag.RowsAffected() == 1 {
			return token, nil
		}
		if !time.Now().Before(deadline) {
			return uuid.Nil, errCompletionBusy
		}
		timer := time.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return uuid.Nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// ReleaseCompletion frees the lease even when the caller has gone away.
func (s *TaskQueueStore) ReleaseCompletion(ctx context.Context, installation uuid.UUID, taskID int64, token uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.pool.Exec(ctx, `DELETE FROM activity_task_completions WHERE installation_id=$1 AND task_id=$2 AND lease_token=$3`, installation, taskID, token)
}

// sweep bounds the queues to employees who used the panel within a day and
// drops completion leases of crashed holders.
func (s *TaskQueueStore) sweep(ctx context.Context) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM activity_task_queues WHERE (installation_id,user_id) IN (SELECT installation_id,user_id FROM activity_task_queues
 WHERE accessed_at<now()-interval '1 day' AND (lease_until IS NULL OR lease_until<now()) LIMIT 100)`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM activity_task_completions WHERE (installation_id,task_id) IN (SELECT installation_id,task_id FROM activity_task_completions
 WHERE lease_until<now() LIMIT 100)`)
}
