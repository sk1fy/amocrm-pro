package leadstatus

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
	"github.com/sk1fy/amocrm-pro/internal/widgetapi"
)

const effectCorrelationTTL = 24 * time.Hour

var ErrExecutionNotAuthorized = widgetapi.ErrExecutionNotAuthorized

type ExecutionStore struct {
	*widgetapi.ExecutionStore
	pool *pgxpool.Pool
}

func NewExecutionStore(pool *pgxpool.Pool) *ExecutionStore {
	return &ExecutionStore{ExecutionStore: widgetapi.NewExecutionStore(pool), pool: pool}
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
