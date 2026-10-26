package gateway

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/corepolicy"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"time"
)

type problemPageAPI interface {
	ReadProblemPage(context.Context, uuid.UUID, string, int) (amocrm.ProblemPage, error)
}

const ProblemRefreshInterval = 7 * time.Minute
const DefaultProblemBatchSize = 5000
const DefaultProblemBatchPause = time.Minute
const problemPageSize = 250

// A batch counts fetched lead and task rows together, across phase boundaries.
// Pause at a page boundary before another full page could exceed BatchSize.
type ProblemCollectionOptions struct {
	BatchSize  int
	BatchPause time.Duration
}
type ProblemStore struct {
	pool    *pgxpool.Pool
	options ProblemCollectionOptions
}

func (s *ProblemStore) batchOptions() ProblemCollectionOptions {
	o := s.options
	if o.BatchSize < problemPageSize {
		o.BatchSize = DefaultProblemBatchSize
	}
	if o.BatchPause <= 0 {
		o.BatchPause = DefaultProblemBatchPause
	}
	return o
}

func (s *Service) WithProblemStore(pool *pgxpool.Pool, options ...ProblemCollectionOptions) *Service {
	s.problemStore = &ProblemStore{pool: pool}
	if len(options) > 0 {
		s.problemStore.options = options[0]
	}
	return s
}
func (s *ProblemStore) Request(ctx context.Context, scope serviceapi.Scope) error {
	// A single UPSERT serializes all tabs. Only an idle, expired snapshot may
	// create a generation. Requests during collection never move its checkpoint.
	var installation uuid.UUID
	err := s.pool.QueryRow(ctx, `INSERT INTO activity_problem_sources(installation_id,integration_id,staging_generation)
 VALUES($1,$2,$3) ON CONFLICT(installation_id) DO UPDATE SET
 staging_generation=excluded.staging_generation, phase='leads', next_page=1,
 batch_records=0,next_poll_at=now(),error_code=''
 WHERE activity_problem_sources.integration_id=excluded.integration_id
 AND activity_problem_sources.phase='idle'
 AND activity_problem_sources.next_poll_at<=now()
 AND (activity_problem_sources.snapshot_at IS NULL OR activity_problem_sources.snapshot_at<=now()-interval '7 minutes')
 RETURNING installation_id`, scope.InstallationID, scope.IntegrationID, uuid.New()).Scan(&installation)
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// Fresh or already collecting: no update, no lease/pause changes. Still verify
	// integration scope rather than treating a conflicting installation as success.
	var exists bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM activity_problem_sources WHERE installation_id=$1 AND integration_id=$2)`, scope.InstallationID, scope.IntegrationID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return serviceapi.Fail(serviceapi.NotFound, "snapshot scope not found")
	}
	return nil
}

func (s *ProblemStore) Read(ctx context.Context, scope serviceapi.Scope, r serviceapi.ProblemRequest) (serviceapi.ProblemSnapshot, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	defer problemRollback(tx)
	var generation *uuid.UUID
	var snapshot *time.Time
	var phase, code string
	var nextPoll time.Time
	err = tx.QueryRow(ctx, `SELECT current_generation,snapshot_at,phase,error_code,next_poll_at FROM activity_problem_sources WHERE installation_id=$1 AND integration_id=$2 FOR SHARE`, scope.InstallationID, scope.IntegrationID).Scan(&generation, &snapshot, &phase, &code, &nextPoll)
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	out := serviceapi.ProblemSnapshot{Counts: []serviceapi.ProblemCount{}, Collecting: phase != "idle", RefreshAfter: nextPoll.Unix(), Stale: true, State: phase, ErrorCode: code}
	if generation == nil || snapshot == nil {
		return out, tx.Commit(ctx)
	}
	out.SnapshotAt = snapshot.Unix()
	out.Stale = time.Since(*snapshot) >= ProblemRefreshInterval || code != ""
	if !out.Collecting && snapshot.Add(ProblemRefreshInterval).After(nextPoll) {
		out.RefreshAfter = snapshot.Add(ProblemRefreshInterval).Unix()
	}
	rows, err := tx.Query(ctx, `SELECT m.id,
 (SELECT count(*) FROM activity_problem_leads l WHERE l.installation_id=$1 AND l.generation=$2 AND l.responsible_user_id=m.id AND NOT l.is_deleted AND l.status_id NOT IN(142,143) AND (cardinality($4::bigint[])=0 OR l.pipeline_id=ANY($4)) AND NOT EXISTS(SELECT 1 FROM activity_problem_tasks t WHERE t.installation_id=l.installation_id AND t.generation=l.generation AND t.entity_type='leads' AND t.entity_id=l.id AND NOT t.is_completed)),
 (SELECT count(*) FROM activity_problem_leads l WHERE l.installation_id=$1 AND l.generation=$2 AND l.responsible_user_id=m.id AND NOT l.is_deleted AND l.status_id NOT IN(142,143) AND (cardinality($4::bigint[])=0 OR l.pipeline_id=ANY($4)) AND EXISTS(SELECT 1 FROM activity_problem_tasks t WHERE t.installation_id=l.installation_id AND t.generation=l.generation AND t.entity_type='leads' AND t.entity_id=l.id AND NOT t.is_completed AND t.complete_till>0 AND t.complete_till<$5)),
 (SELECT count(*) FROM activity_problem_tasks t WHERE t.installation_id=$1 AND t.generation=$2 AND t.responsible_user_id=m.id AND NOT t.is_completed AND t.complete_till>0 AND t.complete_till<$5 AND (cardinality($4::bigint[])=0 OR t.entity_type='leads' AND EXISTS(SELECT 1 FROM activity_problem_leads l WHERE l.installation_id=t.installation_id AND l.generation=t.generation AND l.id=t.entity_id AND NOT l.is_deleted AND l.pipeline_id=ANY($4))))
 FROM unnest($3::bigint[]) WITH ORDINALITY AS m(id,ord) ORDER BY m.ord`, scope.InstallationID, *generation, r.EmployeeIDs, nonNilIDs(r.PipelineIDs), snapshot.Unix())
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	for rows.Next() {
		var c serviceapi.ProblemCount
		if err = rows.Scan(&c.EmployeeID, &c.LeadsNoTasks, &c.LeadsOverdueTasks, &c.OverdueTasks); err != nil {
			rows.Close()
			return serviceapi.ProblemSnapshot{}, err
		}
		out.Counts = append(out.Counts, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return serviceapi.ProblemSnapshot{}, err
	}
	return out, nil
}
func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}
func problemRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

type problemClaim struct {
	scope      serviceapi.Scope
	generation uuid.UUID
	phase      string
	page       int
	token      int64
}

func (s *ProblemStore) claim(ctx context.Context) (problemClaim, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return problemClaim{}, err
	}
	defer problemRollback(tx)
	var c problemClaim
	err = tx.QueryRow(ctx, `SELECT installation_id,integration_id,staging_generation,phase,next_page,lease_token FROM activity_problem_sources WHERE phase<>'idle' AND next_poll_at<=now() AND (lease_until IS NULL OR lease_until<now()) ORDER BY next_poll_at,installation_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&c.scope.InstallationID, &c.scope.IntegrationID, &c.generation, &c.phase, &c.page, &c.token)
	if err != nil {
		return c, err
	}
	c.token++
	_, err = tx.Exec(ctx, `UPDATE activity_problem_sources SET staging_generation=$2,phase=$3,next_page=$4,lease_token=$5,lease_until=now()+interval '30 seconds' WHERE installation_id=$1`, c.scope.InstallationID, c.generation, c.phase, c.page, c.token)
	if err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

// Permanent admission/upstream errors require new viewer demand after a
// cooldown. Only temporary failures retain an automatically resumable phase.
func terminalProblemFailure(err error, code serviceapi.Code) bool {
	switch code {
	case serviceapi.PermissionDenied, serviceapi.NotFound, serviceapi.Unauthenticated, serviceapi.ReauthRequired, serviceapi.InvalidArgument:
		return true
	}
	var upstream *amocrm.APIError
	return errors.As(err, &upstream) && upstream.Kind == amocrm.ErrorPermanent
}
func (s *ProblemStore) fail(ctx context.Context, c problemClaim, err error) {
	mapped := err
	if serviceapi.ErrorCode(err) == serviceapi.Internal {
		mapped = corepolicy.MapUpstreamError(err)
	}
	code := serviceapi.ErrorCode(mapped)
	delay := 30 * time.Second
	phase := c.phase
	if terminalProblemFailure(err, code) {
		phase = "idle"
		delay = ProblemRefreshInterval
	} else {
		var apiError *serviceapi.Error
		if errors.As(mapped, &apiError) && apiError.RetryAfter > delay {
			delay = apiError.RetryAfter
		}
	}
	// The page timeout/cancellation must not prevent the durable retry/stop
	// checkpoint from being saved. Bound this cleanup independently.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, _ = s.pool.Exec(persist, `UPDATE activity_problem_sources SET error_code=$2,lease_until=NULL,next_poll_at=now()+$4::interval,phase=$9 WHERE installation_id=$1 AND integration_id=$5 AND staging_generation=$6 AND phase=$7 AND next_page=$8 AND lease_token=$3`, c.scope.InstallationID, code, c.token, delay.String(), c.scope.IntegrationID, c.generation, c.phase, c.page, phase)
}

func (s *ProblemStore) apply(ctx context.Context, c problemClaim, page amocrm.ProblemPage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer problemRollback(tx)
	var token int64
	var batchRecords int
	err = tx.QueryRow(ctx, `SELECT lease_token,batch_records FROM activity_problem_sources WHERE installation_id=$1 AND integration_id=$2 AND staging_generation=$3 AND phase=$4 AND next_page=$5 FOR UPDATE`, c.scope.InstallationID, c.scope.IntegrationID, c.generation, c.phase, c.page).Scan(&token, &batchRecords)
	if err != nil {
		return err
	}
	if token != c.token {
		return serviceapi.Fail(serviceapi.Conflict, "snapshot lease lost")
	}
	batch := &pgx.Batch{}
	for _, l := range page.Leads {
		batch.Queue(`INSERT INTO activity_problem_leads VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(installation_id,generation,id) DO UPDATE SET responsible_user_id=excluded.responsible_user_id,pipeline_id=excluded.pipeline_id,status_id=excluded.status_id,is_deleted=excluded.is_deleted`, c.scope.InstallationID, c.generation, l.ID, l.ResponsibleUserID, l.PipelineID, l.StatusID, l.IsDeleted)
	}
	for _, t := range page.Tasks {
		batch.Queue(`INSERT INTO activity_problem_tasks VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(installation_id,generation,id) DO UPDATE SET responsible_user_id=excluded.responsible_user_id,entity_id=excluded.entity_id,entity_type=excluded.entity_type,complete_till=excluded.complete_till,is_completed=excluded.is_completed`, c.scope.InstallationID, c.generation, t.ID, t.ResponsibleUserID, t.EntityID, t.EntityType, t.CompleteTill, t.IsCompleted)
	}
	if err = tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	phase, nextPage := c.phase, c.page+1
	if !page.HasNext {
		phase = "tasks"
		nextPage = 1
	}
	if !page.HasNext && c.phase == "tasks" {
		_, err = tx.Exec(ctx, `UPDATE activity_problem_sources SET current_generation=staging_generation,snapshot_at=now(),phase='idle',next_page=1,batch_records=0,lease_until=NULL,next_poll_at=now()+interval '7 minutes',error_code='' WHERE installation_id=$1`, c.scope.InstallationID)
	} else {
		options := s.batchOptions()
		batchRecords += len(page.Leads) + len(page.Tasks)
		delay := time.Duration(0)
		if batchRecords+problemPageSize > options.BatchSize {
			delay = options.BatchPause
			batchRecords = 0
		}
		_, err = tx.Exec(ctx, `UPDATE activity_problem_sources SET phase=$2,next_page=$3,batch_records=$4,lease_until=NULL,next_poll_at=now()+$5::interval,error_code='' WHERE installation_id=$1`, c.scope.InstallationID, phase, nextPage, batchRecords, delay.String())
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Single worker, one <=250-item page per claim; durable leases allow safe restart
// and multiple processes. Large accounts consume more batches, not more memory.
func (s *Service) RunProblemCollector(ctx context.Context) error {
	if s.problemStore == nil {
		return nil
	}
	api, ok := s.api.(problemPageAPI)
	if !ok {
		return serviceapi.Fail(serviceapi.Unavailable, "problem page source unavailable")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		batchCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		c, err := s.problemStore.claim(batchCtx)
		if errors.Is(err, pgx.ErrNoRows) {
			cancel()
			timer.Reset(time.Second)
			continue
		}
		if err == nil {
			err = corepolicy.CheckActivityScope(batchCtx, s.problemStore.pool, c.scope)
			if err == nil {
				var page amocrm.ProblemPage
				page, err = api.ReadProblemPage(batchCtx, c.scope.InstallationID, c.phase, c.page)
				if err == nil {
					err = s.problemStore.apply(batchCtx, c, page)
				}
			}
			if err != nil {
				s.problemStore.fail(batchCtx, c, err)
			}
		}
		cancel()
		// Bounded cleanup: old published generations cannot be confused with current.
		cleanup, cancelCleanup := context.WithTimeout(ctx, 2*time.Second)
		for _, table := range []string{"activity_problem_leads", "activity_problem_tasks"} {
			_, _ = s.problemStore.pool.Exec(cleanup, `DELETE FROM `+table+` WHERE ctid IN(SELECT r.ctid FROM `+table+` r JOIN activity_problem_sources s USING(installation_id) WHERE r.generation<>s.staging_generation AND r.generation IS DISTINCT FROM s.current_generation LIMIT 1000)`)
		}
		cancelCleanup()
		if err != nil {
			timer.Reset(time.Second)
		} else {
			timer.Reset(100 * time.Millisecond)
		}
	}
}
