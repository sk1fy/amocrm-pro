package productplatform

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func (s *LifecycleStore) activation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.Method != http.MethodPost || contentType != "application/json" || r.URL.RawQuery != "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, wire.MaxBody))
	var input wire.ActivationRequest
	if err != nil || wire.Decode(raw, &input) != nil || !validActivation(input) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	result, err := s.storeActivation(ctx, input)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeLifecycleJSON(w, http.StatusOK, result)
}

func validActivation(input wire.ActivationRequest) bool {
	p := input.Scope
	if p.Installation == uuid.Nil || p.Integration == uuid.Nil || p.Account <= 0 ||
		p.Generation <= 0 || p.AuthorizationSequence <= 0 {
		return false
	}
	if c := input.Contact; c != nil {
		phone, err := wire.ContactPhone(c.Phone)
		return err == nil && phone == c.Phone && c.UserID > 0 && c.Consent == wire.ContactConsent &&
			managerText(c.Name, 1, 512) && (c.Email == "" || managerText(c.Email, 1, 320)) &&
			(c.Role == "" || managerText(c.Role, 1, 256))
	}
	return true
}

func (s *LifecycleStore) storeActivation(ctx context.Context, input wire.ActivationRequest) (wire.ActivationResult, error) {
	var result wire.ActivationResult
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	p := input.Scope
	var episode uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT e.id
		FROM platform_lifecycle.cursors c
		JOIN platform_lifecycle.episodes e ON e.installation_id=c.installation_id AND e.generation=c.generation
		JOIN platform_manager.account_memberships m ON m.installation_id=c.installation_id
		WHERE c.installation_id=$1 AND m.integration_id=$2 AND m.amo_account_id=$3
		  AND c.generation=$4 AND c.authorization_sequence=$5
		  AND c.effective_authorization='authorized' AND e.closed_at IS NULL
		FOR UPDATE OF c`, p.Installation, p.Integration, p.Account, p.Generation, p.AuthorizationSequence).Scan(&episode)
	if err != nil {
		return result, err
	}
	if c := input.Contact; c != nil {
		_, err := tx.Exec(ctx, `
			INSERT INTO platform_manager.installation_contacts(episode_id,amo_user_id,name,email,role,phone,consent_version)
			VALUES($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7) ON CONFLICT(episode_id) DO NOTHING`,
			episode, c.UserID, c.Name, c.Email, c.Role, c.Phone, c.Consent)
		if err != nil {
			return result, err
		}
		_, err = tx.Exec(ctx, `
			UPDATE platform_jobs.jobs SET available_at=statement_timestamp()
			WHERE id=$1 AND kind='installation.deal' AND status='queued' AND attempt=0
			  AND available_at>statement_timestamp()`, episode)
		if err != nil {
			return result, err
		}
		result.Activated = true
	} else {
		err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM platform_manager.installation_contacts WHERE episode_id=$1)`, episode).Scan(&result.Activated)
		if err != nil {
			return result, err
		}
	}
	return result, tx.Commit(ctx)
}
