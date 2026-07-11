package widgetapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const (
	PingJobType                    = "widget.ping"
	LeadSetStatusJobType           = "workflow.lead.set_status"
	LeadStatusRuleConfigureJobType = "workflow.rule.lead_status.configure"
	pingIdempotencyScope           = "widget.ping:v1"
	leadStatusScope                = "widget.lead.set_status:v1"
	leadStatusRuleScope            = "widget.workflow_rule.lead_status.configure:v1"
	idempotencyTTL                 = 24 * time.Hour
	staleProcessingAfter           = 5 * time.Minute
	maxIdempotencyKey              = 128
	widgetActorType                = "widget_user"
	leadResourceType               = "lead"
	leadStatusRuleResourceType     = "lead_status_workflow_rule"
)

var (
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrIdempotencyConflict   = errors.New("idempotency key conflicts with another request")
	ErrIdempotencyInProgress = errors.New("idempotent request is still processing")
	ErrInactiveTenant        = errors.New("widget tenant is not active")
	ErrInvalidLeadStatus     = errors.New("invalid lead status command")
	ErrInvalidLeadStatusRule = errors.New("invalid lead status rule command")
)

type ActionResult struct {
	JobID    uuid.UUID   `json:"job_id"`
	Status   jobs.Status `json:"status"`
	Replayed bool        `json:"-"`
}

type LeadStatusCommand struct {
	LeadID     int64 `json:"lead_id"`
	PipelineID int64 `json:"pipeline_id"`
	StatusID   int64 `json:"status_id"`
}

type ActionStore struct {
	pool *pgxpool.Pool
	jobs *jobs.Store
}

type actionAdmission struct {
	principal      widgetauth.Principal
	idempotencyKey string
	scope          string
	requestHash    [sha256.Size]byte
	jobType        string
	resourceType   string
	resourceID     string
	payload        any
	priority       int16
	maxAttempts    int
}

func NewActionStore(pool *pgxpool.Pool, jobStore *jobs.Store) *ActionStore {
	return &ActionStore{pool: pool, jobs: jobStore}
}

// EnqueuePing commits token consumption, durable actor ownership, the
// idempotency outcome, and the job as one PostgreSQL unit.
func (s *ActionStore) EnqueuePing(
	ctx context.Context,
	principal widgetauth.Principal,
	idempotencyKey string,
) (ActionResult, error) {
	return s.enqueue(ctx, actionAdmission{
		principal: principal, idempotencyKey: idempotencyKey,
		scope: pingIdempotencyScope, requestHash: pingRequestHash(principal),
		jobType: PingJobType, payload: map[string]any{}, priority: 50, maxAttempts: 3,
	})
}

// EnqueueLeadSetStatus admits the first real amoCRM workflow. The desired
// state is declarative so a worker retry can compare before writing again.
func (s *ActionStore) EnqueueLeadSetStatus(
	ctx context.Context,
	principal widgetauth.Principal,
	idempotencyKey string,
	command LeadStatusCommand,
) (ActionResult, error) {
	if command.LeadID <= 0 || command.PipelineID <= 0 || command.StatusID <= 0 {
		return ActionResult{}, ErrInvalidLeadStatus
	}
	return s.enqueue(ctx, actionAdmission{
		principal: principal, idempotencyKey: idempotencyKey,
		scope: leadStatusScope, requestHash: leadStatusRequestHash(principal, command),
		jobType: LeadSetStatusJobType, resourceType: leadResourceType,
		resourceID: strconv.FormatInt(command.LeadID, 10), payload: command,
		priority: 40, maxAttempts: 5,
	})
}

func (s *ActionStore) enqueue(ctx context.Context, admission actionAdmission) (ActionResult, error) {
	if s == nil || s.pool == nil || s.jobs == nil {
		return ActionResult{}, errors.New("widget action store is not configured")
	}
	if !validIdempotencyKey(admission.idempotencyKey) {
		return ActionResult{}, ErrInvalidIdempotencyKey
	}
	if err := validatePrincipal(admission.principal); err != nil {
		return ActionResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ActionResult{}, fmt.Errorf("begin widget action: %w", err)
	}
	defer func() { _ = rollbackActionTransaction(tx) }()

	if err := lockActiveInstallation(ctx, tx, admission.principal); err != nil {
		return ActionResult{}, err
	}
	if err := consumeActionToken(ctx, tx, admission.principal); err != nil {
		return ActionResult{}, err
	}

	keyHash := sha256.Sum256([]byte(admission.idempotencyKey))
	idempotencyID := uuid.New()
	now := time.Now().UTC()
	expiresAt := now.Add(idempotencyTTL)
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (
			id, installation_id, scope, key_hash, request_hash, status, expires_at
		) VALUES ($1, $2, $3, $4, $5, 'processing', $6)
		ON CONFLICT (installation_id, scope, key_hash) DO NOTHING`,
		idempotencyID, admission.principal.InstallationID, admission.scope,
		keyHash[:], admission.requestHash[:], expiresAt,
	)
	if err != nil {
		return ActionResult{}, fmt.Errorf("claim widget idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return s.createAction(ctx, tx, idempotencyID, admission)
	}

	var (
		existingID      uuid.UUID
		existingHash    []byte
		status          string
		storedJobID     *uuid.UUID
		responseStatus  *int
		responseBody    []byte
		existingExpires time.Time
		createdAt       time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, request_hash, status, job_id, response_status, response_body,
			expires_at, created_at
		FROM idempotency_keys
		WHERE installation_id=$1 AND scope=$2 AND key_hash=$3
		FOR UPDATE`,
		admission.principal.InstallationID, admission.scope, keyHash[:],
	).Scan(
		&existingID, &existingHash, &status, &storedJobID, &responseStatus,
		&responseBody, &existingExpires, &createdAt,
	)
	if err != nil {
		return ActionResult{}, fmt.Errorf("read widget idempotency result: %w", err)
	}

	if !existingExpires.After(now) ||
		(status == "processing" && bytes.Equal(existingHash, admission.requestHash[:]) && createdAt.Before(now.Add(-staleProcessingAfter))) {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE id=$1`, existingID); err != nil {
			return ActionResult{}, fmt.Errorf("delete reclaimable widget idempotency key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (
				id, installation_id, scope, key_hash, request_hash, status, expires_at
			) VALUES ($1, $2, $3, $4, $5, 'processing', $6)`,
			idempotencyID, admission.principal.InstallationID, admission.scope,
			keyHash[:], admission.requestHash[:], expiresAt,
		); err != nil {
			return ActionResult{}, fmt.Errorf("reclaim widget idempotency key: %w", err)
		}
		return s.createAction(ctx, tx, idempotencyID, admission)
	}
	if !bytes.Equal(existingHash, admission.requestHash[:]) {
		if err := tx.Commit(ctx); err != nil {
			return ActionResult{}, fmt.Errorf("commit conflicting widget action token: %w", err)
		}
		return ActionResult{}, ErrIdempotencyConflict
	}
	if status != "completed" || storedJobID == nil || responseStatus == nil ||
		*responseStatus != 202 || len(responseBody) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return ActionResult{}, fmt.Errorf("commit pending widget action token: %w", err)
		}
		return ActionResult{}, ErrIdempotencyInProgress
	}

	var result ActionResult
	if err := json.Unmarshal(responseBody, &result); err != nil ||
		result.JobID == uuid.Nil || result.JobID != *storedJobID {
		return ActionResult{}, errors.New("stored widget idempotency response is invalid")
	}
	result.Replayed = true
	if err := tx.Commit(ctx); err != nil {
		return ActionResult{}, fmt.Errorf("commit replayed widget action: %w", err)
	}
	return result, nil
}

func lockActiveInstallation(ctx context.Context, tx pgx.Tx, principal widgetauth.Principal) error {
	var installationID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT installation.id
		FROM installations AS installation
		JOIN integrations AS integration ON integration.id=installation.integration_id
		WHERE installation.id=$1 AND installation.integration_id=$2
		  AND installation.account_id=$3 AND installation.status='active'
		  AND integration.status='active'
		FOR SHARE OF installation, integration`,
		principal.InstallationID, principal.IntegrationID, principal.AccountID,
	).Scan(&installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInactiveTenant
	}
	if err != nil {
		return fmt.Errorf("lock widget installation: %w", err)
	}
	return nil
}

func consumeActionToken(ctx context.Context, tx pgx.Tx, principal widgetauth.Principal) error {
	used := principal.UsedToken()
	tag, err := tx.Exec(ctx, `
		INSERT INTO used_widget_tokens (
			integration_id, jti, issuer, account_id, user_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (integration_id, jti) DO NOTHING`,
		used.IntegrationID, used.TokenID, used.Issuer, used.AccountID, used.UserID, used.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("consume widget action token: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return widgetauth.ErrReplay
	}
	return nil
}

func (s *ActionStore) createAction(
	ctx context.Context,
	tx pgx.Tx,
	idempotencyID uuid.UUID,
	admission actionAdmission,
) (ActionResult, error) {
	job, err := s.jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &admission.principal.InstallationID,
		Type:           admission.jobType, ActorType: widgetActorType,
		ActorID:      strconv.FormatInt(admission.principal.UserID, 10),
		ResourceType: admission.resourceType, ResourceID: admission.resourceID,
		Priority: admission.priority, MaxAttempts: admission.maxAttempts,
		Payload: admission.payload,
	})
	if err != nil {
		return ActionResult{}, fmt.Errorf("enqueue idempotent widget action: %w", err)
	}
	result := ActionResult{JobID: job.ID, Status: job.Status}
	responseBody, err := json.Marshal(result)
	if err != nil {
		return ActionResult{}, fmt.Errorf("marshal widget action response: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET status='completed', job_id=$2, response_status=202, response_body=$3
		WHERE id=$1 AND status='processing'`, idempotencyID, job.ID, responseBody)
	if err != nil {
		return ActionResult{}, fmt.Errorf("complete widget idempotency result: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ActionResult{}, errors.New("widget idempotency claim was lost")
	}
	if err := tx.Commit(ctx); err != nil {
		return ActionResult{}, fmt.Errorf("commit widget action: %w", err)
	}
	return result, nil
}

func validIdempotencyKey(value string) bool {
	if value == "" || len(value) > maxIdempotencyKey || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validatePrincipal(principal widgetauth.Principal) error {
	if principal.IntegrationID == uuid.Nil || principal.InstallationID == uuid.Nil ||
		principal.AccountID <= 0 || principal.UserID <= 0 || principal.TokenID == "" ||
		principal.Issuer == "" || !principal.TokenRetainUntil.After(time.Now().UTC()) {
		return errors.New("verified widget principal is incomplete or expired")
	}
	return nil
}

func pingRequestHash(principal widgetauth.Principal) [sha256.Size]byte {
	canonical := fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%s",
		pingIdempotencyScope, principal.InstallationID,
		principal.AccountID, principal.UserID, principal.ClientUUID,
	)
	return sha256.Sum256([]byte(canonical))
}

func leadStatusRequestHash(principal widgetauth.Principal, command LeadStatusCommand) [sha256.Size]byte {
	canonical := fmt.Sprintf(
		"%s\x00%s\x00%d\x00%d\x00%s\x00%d\x00%d\x00%d",
		leadStatusScope, principal.InstallationID, principal.AccountID,
		principal.UserID, principal.ClientUUID, command.LeadID,
		command.PipelineID, command.StatusID,
	)
	return sha256.Sum256([]byte(canonical))
}

func rollbackActionTransaction(tx pgx.Tx) error {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := tx.Rollback(rollbackContext)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}
