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
	"github.com/sk1fy/amocrm-pro/internal/services"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
)

const (
	PingJobType          = "widget.ping"
	pingIdempotencyScope = "widget.ping:v1"
	idempotencyTTL       = 24 * time.Hour
	staleProcessingAfter = 5 * time.Minute
	maxIdempotencyKey    = 128
	widgetActorType      = "widget_user"
)

var (
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrIdempotencyConflict   = errors.New("idempotency key conflicts with another request")
	ErrIdempotencyInProgress = errors.New("idempotent request is still processing")
	ErrInactiveTenant        = errors.New("widget tenant is not active")
)

type ActionResult struct {
	JobID    uuid.UUID   `json:"job_id"`
	Status   jobs.Status `json:"status"`
	Replayed bool        `json:"-"`
}

type ActionStore struct {
	pool *pgxpool.Pool
	jobs *jobs.Store
}

// ActionAdmission is a module-validated command and its stable durable identity.
// Product modules own payload validation, request hashing and idempotency scope;
// Enqueue owns tenant/capability authorization, token consumption and atomicity.
type ActionAdmission struct {
	Principal      widgetauth.Principal
	IdempotencyKey string
	Scope          string
	RequestHash    [sha256.Size]byte
	JobType        string
	ResourceType   string
	ResourceID     string
	Payload        any
	Priority       int16
	MaxAttempts    int
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
	return s.Enqueue(ctx, ActionAdmission{
		Principal: principal, IdempotencyKey: idempotencyKey,
		Scope: pingIdempotencyScope, RequestHash: pingRequestHash(principal),
		JobType: PingJobType, Payload: map[string]any{}, Priority: 50, MaxAttempts: 3,
	})
}

func (s *ActionStore) Enqueue(ctx context.Context, admission ActionAdmission) (ActionResult, error) {
	if s == nil || s.pool == nil || s.jobs == nil {
		return ActionResult{}, errors.New("widget action store is not configured")
	}
	if !validIdempotencyKey(admission.IdempotencyKey) {
		return ActionResult{}, ErrInvalidIdempotencyKey
	}
	if err := validatePrincipal(admission.Principal); err != nil {
		return ActionResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ActionResult{}, fmt.Errorf("begin widget action: %w", err)
	}
	defer func() { _ = rollbackActionTransaction(tx) }()

	if err := lockActiveInstallation(ctx, tx, admission.Principal); err != nil {
		return ActionResult{}, err
	}
	serviceCode, known := services.JobService(admission.JobType)
	if !known {
		return ActionResult{}, services.ErrNotEnabled
	}
	if serviceCode != "" {
		if err := services.RequireEnabled(ctx, tx, admission.Principal.InstallationID, serviceCode, true); err != nil {
			return ActionResult{}, err
		}
	}
	if err := consumeActionToken(ctx, tx, admission.Principal); err != nil {
		return ActionResult{}, err
	}

	keyHash := sha256.Sum256([]byte(admission.IdempotencyKey))
	idempotencyID := uuid.New()
	now := time.Now().UTC()
	expiresAt := now.Add(idempotencyTTL)
	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (
			id, installation_id, scope, key_hash, request_hash, status, expires_at
		) VALUES ($1, $2, $3, $4, $5, 'processing', $6)
		ON CONFLICT (installation_id, scope, key_hash) DO NOTHING`,
		idempotencyID, admission.Principal.InstallationID, admission.Scope,
		keyHash[:], admission.RequestHash[:], expiresAt,
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
		admission.Principal.InstallationID, admission.Scope, keyHash[:],
	).Scan(
		&existingID, &existingHash, &status, &storedJobID, &responseStatus,
		&responseBody, &existingExpires, &createdAt,
	)
	if err != nil {
		return ActionResult{}, fmt.Errorf("read widget idempotency result: %w", err)
	}

	if !existingExpires.After(now) ||
		(status == "processing" && bytes.Equal(existingHash, admission.RequestHash[:]) && createdAt.Before(now.Add(-staleProcessingAfter))) {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE id=$1`, existingID); err != nil {
			return ActionResult{}, fmt.Errorf("delete reclaimable widget idempotency key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (
				id, installation_id, scope, key_hash, request_hash, status, expires_at
			) VALUES ($1, $2, $3, $4, $5, 'processing', $6)`,
			idempotencyID, admission.Principal.InstallationID, admission.Scope,
			keyHash[:], admission.RequestHash[:], expiresAt,
		); err != nil {
			return ActionResult{}, fmt.Errorf("reclaim widget idempotency key: %w", err)
		}
		return s.createAction(ctx, tx, idempotencyID, admission)
	}
	if !bytes.Equal(existingHash, admission.RequestHash[:]) {
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
	admission ActionAdmission,
) (ActionResult, error) {
	job, err := s.jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &admission.Principal.InstallationID,
		Type:           admission.JobType, ActorType: widgetActorType,
		ActorID:      strconv.FormatInt(admission.Principal.UserID, 10),
		ResourceType: admission.ResourceType, ResourceID: admission.ResourceID,
		Priority: admission.Priority, MaxAttempts: admission.MaxAttempts,
		Payload: admission.Payload,
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

func rollbackActionTransaction(tx pgx.Tx) error {
	rollbackContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := tx.Rollback(rollbackContext)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}
