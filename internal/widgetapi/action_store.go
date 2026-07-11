package widgetapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	pingIdempotencyScope = "widget.ping:v1"
	idempotencyTTL       = 24 * time.Hour
	maxIdempotencyKey    = 128
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

func NewActionStore(pool *pgxpool.Pool, jobStore *jobs.Store) *ActionStore {
	return &ActionStore{pool: pool, jobs: jobStore}
}

// EnqueuePing commits token consumption, the idempotency outcome, and the job
// as one unit. A retry after an uncertain HTTP response must use a fresh JWT
// and the same idempotency key.
func (s *ActionStore) EnqueuePing(
	ctx context.Context,
	principal widgetauth.Principal,
	idempotencyKey string,
) (ActionResult, error) {
	if s == nil || s.pool == nil || s.jobs == nil {
		return ActionResult{}, errors.New("widget action store is not configured")
	}
	if !validIdempotencyKey(idempotencyKey) {
		return ActionResult{}, ErrInvalidIdempotencyKey
	}
	if err := validatePrincipal(principal); err != nil {
		return ActionResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ActionResult{}, fmt.Errorf("begin widget action: %w", err)
	}
	defer func() { _ = rollbackActionTransaction(tx) }()

	// The trusted tenant is locked first so disable/uninstall cannot race the
	// admission transaction after JWT verification.
	var installationID uuid.UUID
	err = tx.QueryRow(ctx, `
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
		return ActionResult{}, ErrInactiveTenant
	}
	if err != nil {
		return ActionResult{}, fmt.Errorf("lock widget installation: %w", err)
	}

	used := principal.UsedToken()
	tag, err := tx.Exec(ctx, `
		INSERT INTO used_widget_tokens (
			integration_id, jti, issuer, account_id, user_id, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (integration_id, jti) DO NOTHING`,
		used.IntegrationID, used.TokenID, used.Issuer, used.AccountID, used.UserID, used.ExpiresAt,
	)
	if err != nil {
		return ActionResult{}, fmt.Errorf("consume widget action token: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ActionResult{}, widgetauth.ErrReplay
	}

	keyHash := sha256.Sum256([]byte(idempotencyKey))
	requestHash := pingRequestHash(principal)
	idempotencyID := uuid.New()
	expiresAt := time.Now().UTC().Add(idempotencyTTL)
	tag, err = tx.Exec(ctx, `
		INSERT INTO idempotency_keys (
			id, installation_id, scope, key_hash, request_hash, status, expires_at
		) VALUES ($1, $2, $3, $4, $5, 'processing', $6)
		ON CONFLICT (installation_id, scope, key_hash) DO NOTHING`,
		idempotencyID, principal.InstallationID, pingIdempotencyScope,
		keyHash[:], requestHash[:], expiresAt,
	)
	if err != nil {
		return ActionResult{}, fmt.Errorf("claim widget idempotency key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return s.createPing(ctx, tx, idempotencyID, principal)
	}

	var (
		existingID      uuid.UUID
		existingHash    []byte
		status          string
		responseStatus  *int
		responseBody    []byte
		existingExpires time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT id, request_hash, status, response_status, response_body, expires_at
		FROM idempotency_keys
		WHERE installation_id=$1 AND scope=$2 AND key_hash=$3
		FOR UPDATE`,
		principal.InstallationID, pingIdempotencyScope, keyHash[:],
	).Scan(&existingID, &existingHash, &status, &responseStatus, &responseBody, &existingExpires)
	if err != nil {
		return ActionResult{}, fmt.Errorf("read widget idempotency result: %w", err)
	}
	if !existingExpires.After(time.Now().UTC()) {
		if _, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE id=$1`, existingID); err != nil {
			return ActionResult{}, fmt.Errorf("delete expired widget idempotency key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (
				id, installation_id, scope, key_hash, request_hash, status, expires_at
			) VALUES ($1, $2, $3, $4, $5, 'processing', $6)`,
			idempotencyID, principal.InstallationID, pingIdempotencyScope,
			keyHash[:], requestHash[:], expiresAt,
		); err != nil {
			return ActionResult{}, fmt.Errorf("reclaim expired widget idempotency key: %w", err)
		}
		return s.createPing(ctx, tx, idempotencyID, principal)
	}
	if !bytes.Equal(existingHash, requestHash[:]) {
		if err := tx.Commit(ctx); err != nil {
			return ActionResult{}, fmt.Errorf("commit conflicting widget action token: %w", err)
		}
		return ActionResult{}, ErrIdempotencyConflict
	}
	if status != "completed" || responseStatus == nil || *responseStatus != 202 || len(responseBody) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return ActionResult{}, fmt.Errorf("commit pending widget action token: %w", err)
		}
		return ActionResult{}, ErrIdempotencyInProgress
	}

	var result ActionResult
	if err := json.Unmarshal(responseBody, &result); err != nil || result.JobID == uuid.Nil {
		return ActionResult{}, errors.New("stored widget idempotency response is invalid")
	}
	result.Replayed = true
	if err := tx.Commit(ctx); err != nil {
		return ActionResult{}, fmt.Errorf("commit replayed widget action: %w", err)
	}
	return result, nil
}

func (s *ActionStore) createPing(
	ctx context.Context,
	tx pgx.Tx,
	idempotencyID uuid.UUID,
	principal widgetauth.Principal,
) (ActionResult, error) {
	job, err := s.jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &principal.InstallationID,
		Type:           "widget.ping",
		Priority:       50,
		MaxAttempts:    3,
		Payload: map[string]any{
			"account_id": principal.AccountID,
			"user_id":    principal.UserID,
		},
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
		pingIdempotencyScope,
		principal.InstallationID,
		principal.AccountID,
		principal.UserID,
		principal.ClientUUID,
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
