package widgetapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

var ErrExecutionNotAuthorized = errors.New("widget action execution is not authorized")

const mutationLeaseFloor = 2 * time.Minute

type ExecutionStore struct {
	pool *pgxpool.Pool
}

func NewExecutionStore(pool *pgxpool.Pool) *ExecutionStore {
	return &ExecutionStore{pool: pool}
}

// Authorize re-reads durable actor/resource ownership and active tenant state.
// Real workflows call it once before remote reads and again immediately before
// a remote mutation, narrowing the disable/uninstall race without holding a
// PostgreSQL connection across OAuth refresh or external I/O.
func (s *ExecutionStore) Authorize(
	ctx context.Context,
	job jobs.Job,
	expectedType string,
	expectedResourceType string,
) error {
	if s == nil || s.pool == nil || job.InstallationID == nil ||
		job.ActorType == nil || job.ActorID == nil ||
		job.LockedBy == nil || job.LockedUntil == nil ||
		job.Type != expectedType || *job.ActorType != widgetActorType {
		return ErrExecutionNotAuthorized
	}
	if expectedResourceType != "" {
		if job.ResourceType == nil || job.ResourceID == nil || *job.ResourceType != expectedResourceType {
			return ErrExecutionNotAuthorized
		}
	}
	var marker int
	err := s.pool.QueryRow(ctx, `
		SELECT 1
		FROM jobs AS job
		JOIN installations AS installation ON installation.id=job.installation_id
		JOIN integrations AS integration ON integration.id=installation.integration_id
		WHERE job.id=$1 AND job.installation_id=$2 AND job.type=$3
		  AND job.status='processing'
		  AND job.attempts=$4 AND job.locked_by=$5 AND job.locked_until >= now()
		  AND job.actor_type=$6 AND job.actor_id=$7
		  AND job.resource_type IS NOT DISTINCT FROM $8
		  AND job.resource_id IS NOT DISTINCT FROM $9
		  AND installation.status='active' AND integration.status='active'`,
		job.ID, *job.InstallationID, job.Type, job.Attempts, *job.LockedBy,
		*job.ActorType, *job.ActorID, job.ResourceType, job.ResourceID,
	).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionNotAuthorized
	}
	if err != nil {
		return fmt.Errorf("authorize widget action execution: %w", err)
	}
	return nil
}

// WithMutationAuthorization establishes the ordering point between an active
// installation and its external mutation. A concurrent disable/uninstall must
// wait for this transaction; if lifecycle state changed first, the callback is
// never invoked. The callback must remain bounded by the job context.
func (s *ExecutionStore) WithMutationAuthorization(
	ctx context.Context,
	job jobs.Job,
	expectedType string,
	expectedResourceType string,
	callback func(context.Context) error,
) error {
	if s == nil || s.pool == nil || callback == nil || job.InstallationID == nil ||
		job.ActorType == nil || job.ActorID == nil || job.ResourceType == nil ||
		job.ResourceID == nil || job.LockedBy == nil || job.LockedUntil == nil ||
		job.Type != expectedType || *job.ActorType != widgetActorType ||
		*job.ResourceType != expectedResourceType {
		return ErrExecutionNotAuthorized
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET locked_until=GREATEST(locked_until, now()+($6 * interval '1 millisecond'))
		WHERE id=$1 AND status='processing' AND attempts=$2
		  AND locked_by=$3 AND locked_until >= now()
		  AND installation_id=$4 AND type=$5`,
		job.ID, job.Attempts, *job.LockedBy, *job.InstallationID,
		expectedType, mutationLeaseFloor.Milliseconds(),
	)
	if err != nil {
		return fmt.Errorf("extend mutation lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrExecutionNotAuthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mutation authorization: %w", err)
	}
	defer func() {
		rollbackContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackContext)
	}()
	var marker int
	err = tx.QueryRow(ctx, `
		SELECT 1
		FROM jobs AS job
		JOIN installations AS installation ON installation.id=job.installation_id
		JOIN integrations AS integration ON integration.id=installation.integration_id
		WHERE job.id=$1 AND job.installation_id=$2 AND job.type=$3
		  AND job.status='processing'
		  AND job.attempts=$4 AND job.locked_by=$5 AND job.locked_until >= now()
		  AND job.actor_type=$6 AND job.actor_id=$7
		  AND job.resource_type=$8 AND job.resource_id=$9
		  AND installation.status='active' AND integration.status='active'
		FOR SHARE OF installation, integration`,
		job.ID, *job.InstallationID, expectedType, job.Attempts, *job.LockedBy,
		*job.ActorType, *job.ActorID, expectedResourceType, *job.ResourceID,
	).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionNotAuthorized
	}
	if err != nil {
		return fmt.Errorf("lock mutation authorization: %w", err)
	}
	if err := callback(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mutation authorization: %w", err)
	}
	return nil
}

func jobActorUserID(job jobs.Job) (int64, error) {
	if job.ActorType == nil || job.ActorID == nil || *job.ActorType != widgetActorType {
		return 0, ErrExecutionNotAuthorized
	}
	value, err := strconv.ParseInt(*job.ActorID, 10, 64)
	if err != nil || value <= 0 {
		return 0, ErrExecutionNotAuthorized
	}
	return value, nil
}

func jobLeadID(job jobs.Job) (int64, error) {
	if job.ResourceType == nil || job.ResourceID == nil || *job.ResourceType != leadResourceType {
		return 0, ErrExecutionNotAuthorized
	}
	value, err := strconv.ParseInt(*job.ResourceID, 10, 64)
	if err != nil || value <= 0 {
		return 0, ErrExecutionNotAuthorized
	}
	return value, nil
}

func installationID(job jobs.Job) (uuid.UUID, error) {
	if job.InstallationID == nil || *job.InstallationID == uuid.Nil {
		return uuid.Nil, ErrExecutionNotAuthorized
	}
	return *job.InstallationID, nil
}
