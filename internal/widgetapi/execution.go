package widgetapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/platform/sanitize"
)

var ErrExecutionNotAuthorized = errors.New("widget action execution is not authorized")

const mutationLeaseFloor = 2 * time.Minute

const effectCorrelationTTL = 24 * time.Hour

type ExecutionStore struct {
	pool *pgxpool.Pool
}

func NewExecutionStore(pool *pgxpool.Pool) *ExecutionStore {
	return &ExecutionStore{pool: pool}
}

// LeadStatusEffectHash is shared with webhook correlation. It deliberately
// hashes only the non-sensitive, convergent desired state.
func LeadStatusEffectHash(pipelineID, statusID int64) [sha256.Size]byte {
	var input [16]byte
	binary.BigEndian.PutUint64(input[:8], uint64(pipelineID))
	binary.BigEndian.PutUint64(input[8:], uint64(statusID))
	return sha256.Sum256(input[:])
}

func (s *ExecutionStore) PrepareLeadStatusEffect(
	ctx context.Context,
	job jobs.Job,
	workflowRunID *uuid.UUID,
	leadID int64,
	pipelineID int64,
	statusID int64,
) (uuid.UUID, error) {
	if s == nil || s.pool == nil || job.InstallationID == nil ||
		leadID <= 0 || pipelineID <= 0 || statusID <= 0 {
		return uuid.Nil, ErrExecutionNotAuthorized
	}
	desiredState, _ := json.Marshal(map[string]int64{
		"pipeline_id": pipelineID, "status_id": statusID,
	})
	desiredHash := LeadStatusEffectHash(pipelineID, statusID)
	var effectID uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO outbound_effects (
			installation_id, workflow_run_id, correlation_job_id, effect_type,
			resource_type, resource_id, desired_state, desired_hash,
			correlation_expires_at
		) VALUES ($1, $2, $3, 'lead.set_status', 'lead', $4, $5, $6,
			now()+($7 * interval '1 millisecond'))
		ON CONFLICT (correlation_job_id) DO UPDATE
		SET state=CASE
				WHEN outbound_effects.state IN ('failed', 'expired', 'no_effect') THEN 'prepared'
				ELSE outbound_effects.state
			END,
			attempted_at=CASE
				WHEN outbound_effects.state IN ('prepared', 'uncertain') THEN now()
				WHEN outbound_effects.state IN ('failed', 'expired', 'no_effect') THEN now()
				ELSE outbound_effects.attempted_at
			END,
			correlation_expires_at=GREATEST(
				outbound_effects.correlation_expires_at,
				now()+($7 * interval '1 millisecond')
			),
			updated_at=now()
		RETURNING id`,
		*job.InstallationID, workflowRunID, job.ID, strconv.FormatInt(leadID, 10),
		desiredState, desiredHash[:], effectCorrelationTTL.Milliseconds(),
	).Scan(&effectID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("prepare lead status effect: %w", err)
	}
	return effectID, nil
}

func (s *ExecutionStore) MarkLeadStatusEffect(
	ctx context.Context,
	effectID uuid.UUID,
	state string,
	effectErr error,
) error {
	message := ""
	if effectErr != nil {
		message = sanitize.Text(effectErr.Error(), 4000)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE outbound_effects
		SET state=CASE WHEN state='observed' THEN state ELSE $2 END,
			applied_at=CASE
				WHEN $2='applied' AND applied_at IS NULL THEN now()
				ELSE applied_at
			END,
			last_error=NULLIF($3, ''), updated_at=now()
		WHERE id=$1`, effectID, state, message,
	)
	if err != nil {
		return fmt.Errorf("mark outbound effect %s: %w", state, err)
	}
	return nil
}

func (s *ExecutionStore) LeadStatusEffectForJob(
	ctx context.Context,
	jobID uuid.UUID,
) (uuid.UUID, bool, error) {
	var effectID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM outbound_effects WHERE correlation_job_id=$1`, jobID).Scan(&effectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("load outbound effect: %w", err)
	}
	return effectID, true, nil
}

// AuthorizeIntegrationAction fences a webhook-origin action to its current
// worker lease and active installation/integration. Unlike widget actions it
// deliberately carries no end-user authorization claim.
func (s *ExecutionStore) AuthorizeIntegrationAction(
	ctx context.Context,
	job jobs.Job,
	expectedType string,
	expectedResourceType string,
) error {
	if s == nil || s.pool == nil || job.InstallationID == nil ||
		job.ActorType == nil || job.ActorID == nil ||
		job.ResourceType == nil || job.ResourceID == nil ||
		job.LockedBy == nil || job.LockedUntil == nil ||
		job.Type != expectedType || *job.ActorType != "integration" ||
		*job.ActorID != job.InstallationID.String() ||
		*job.ResourceType != expectedResourceType {
		return ErrExecutionNotAuthorized
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
		  AND job.actor_type='integration' AND job.actor_id=$6
		  AND job.resource_type=$7 AND job.resource_id=$8
		  AND installation.status='active' AND integration.status='active'`,
		job.ID, *job.InstallationID, expectedType, job.Attempts, *job.LockedBy,
		*job.ActorID, expectedResourceType, *job.ResourceID,
	).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionNotAuthorized
	}
	if err != nil {
		return fmt.Errorf("authorize integration action execution: %w", err)
	}
	return nil
}

// WithIntegrationMutationAuthorization provides the same lifecycle ordering
// point as WithMutationAuthorization without asserting a widget user actor.
func (s *ExecutionStore) WithIntegrationMutationAuthorization(
	ctx context.Context,
	job jobs.Job,
	expectedType string,
	expectedResourceType string,
	callback func(context.Context) error,
) error {
	if callback == nil {
		return ErrExecutionNotAuthorized
	}
	if err := s.AuthorizeIntegrationAction(ctx, job, expectedType, expectedResourceType); err != nil {
		return err
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
		return fmt.Errorf("extend integration mutation lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrExecutionNotAuthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin integration mutation authorization: %w", err)
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
		  AND job.actor_type='integration' AND job.actor_id=$6
		  AND job.resource_type=$7 AND job.resource_id=$8
		  AND installation.status='active' AND integration.status='active'
		FOR SHARE OF installation, integration`,
		job.ID, *job.InstallationID, expectedType, job.Attempts, *job.LockedBy,
		*job.ActorID, expectedResourceType, *job.ResourceID,
	).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionNotAuthorized
	}
	if err != nil {
		return fmt.Errorf("lock integration mutation authorization: %w", err)
	}
	if err := callback(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit integration mutation authorization: %w", err)
	}
	return nil
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
