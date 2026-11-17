package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const reviewCommandKind = "installation.review"

type reviewInput struct {
	Reviewed bool    `json:"reviewed"`
	Reason   *string `json:"reason,omitempty"`
}

type reviewCommand struct {
	ID, Key, Episode, Subject uuid.UUID
	RequestID                 uuid.UUID
	Version                   int64
	Input                     reviewInput
}

type managerOutcome struct {
	status int
	body   []byte
	etag   *string
}

func (s managerStore) review(ctx context.Context, cmd reviewCommand) (managerOutcome, error) {
	var outcome managerOutcome
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return outcome, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var coreInstallation uuid.UUID
	err = tx.QueryRow(ctx, `SELECT installation_id FROM platform_manager.installations WHERE id=$1`, cmd.Episode).Scan(&coreInstallation)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcome, errManagerNotFound
	}
	if err != nil {
		return outcome, err
	}
	input, _ := json.Marshal(cmd.Input)
	bodyHash := sha256.Sum256(input)
	reserved, err := tx.Exec(ctx, `INSERT INTO platform_manager.commands
		(id,subject_id,idempotency_key,kind,episode_id,expected_version,body_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`,
		cmd.ID, cmd.Subject, cmd.Key, reviewCommandKind, cmd.Episode, cmd.Version, bodyHash[:])
	if err != nil {
		return outcome, err
	}
	if reserved.RowsAffected() == 0 {
		return replayReview(ctx, tx, cmd, bodyHash)
	}
	if _, err := lockStream(ctx, tx, coreInstallation); err != nil {
		return outcome, err
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return outcome, err
	}
	now = now.UTC()
	item, err := scanManagerInstallation(tx.QueryRow(ctx,
		`SELECT `+installationColumns+` FROM platform_manager.installations WHERE id=$1`, cmd.Episode), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcome, errManagerNotFound
	}
	if err != nil {
		return outcome, err
	}

	var response any
	if item.Version != cmd.Version {
		outcome.status = http.StatusConflict
		response = managerProblem(http.StatusConflict, "version_conflict", cmd.RequestID, item.Version, 0)
	} else {
		if item.Review.Reviewed != cmd.Input.Reviewed {
			if err := applyReview(ctx, tx, &item, cmd, now); err != nil {
				return outcome, err
			}
		}
		etag := strconv.Quote(strconv.FormatInt(item.Version, 10))
		outcome.status, outcome.etag, response = http.StatusOK, &etag, item
	}
	outcome.body, err = json.Marshal(response)
	if err != nil {
		return outcome, err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_manager.commands SET status=$2,response=$3,etag=$4 WHERE id=$1`,
		cmd.ID, outcome.status, outcome.body, outcome.etag); err != nil {
		return outcome, err
	}
	return outcome, tx.Commit(ctx)
}

func replayReview(ctx context.Context, tx pgx.Tx, cmd reviewCommand, bodyHash [32]byte) (managerOutcome, error) {
	var outcome managerOutcome
	var status *int
	err := tx.QueryRow(ctx, `
		SELECT c.status,c.response,c.etag FROM platform_manager.installations i
		LEFT JOIN platform_manager.commands c ON c.id=$1 AND c.subject_id=$2 AND c.idempotency_key=$3
			AND c.kind=$4 AND c.episode_id=i.id AND c.expected_version=$6 AND c.body_hash=$7
		WHERE i.id=$5`, cmd.ID, cmd.Subject, cmd.Key, reviewCommandKind, cmd.Episode, cmd.Version, bodyHash[:]).
		Scan(&status, &outcome.body, &outcome.etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return outcome, errManagerNotFound
	}
	if err != nil {
		return outcome, err
	}
	if status == nil {
		return outcome, errManagerConflict
	}
	outcome.status = *status
	return outcome, tx.Commit(ctx)
}

func applyReview(ctx context.Context, tx pgx.Tx, item *managerInstallation, cmd reviewCommand, now time.Time) error {
	kind := "installation_unreviewed"
	item.Review = installationReview{}
	if cmd.Input.Reviewed {
		kind = "installation_reviewed"
		item.Review = installationReview{Reviewed: true, By: &cmd.Subject, At: &now}
	}
	err := tx.QueryRow(ctx, `
		UPDATE platform_lifecycle.episodes SET version=version+1,reviewed_by=$2,reviewed_at=$3 WHERE id=$1
		RETURNING version`, cmd.Episode, item.Review.By, item.Review.At).Scan(&item.Version)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH event AS (
			INSERT INTO platform_manager.review_events
				(id,command_id,episode_id,account_id,product_id,kind,actor_id,occurred_at,reason,version)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING id,kind,episode_id,account_id,product_id,actor_id,occurred_at,recorded_at,version
		)
		INSERT INTO platform_manager.event_outbox(event_id,payload) SELECT id,to_jsonb(event) FROM event`,
		uuid.New(), cmd.ID, cmd.Episode, item.Account, item.Product, kind, cmd.Subject, now, cmd.Input.Reason, item.Version)
	return err
}

type managerReceipt struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"`
	State string    `json:"state"`
}

func (s managerStore) receipt(ctx context.Context, id, subject uuid.UUID) (managerReceipt, error) {
	var receipt managerReceipt
	err := s.pool.QueryRow(ctx, `
		SELECT c.id,c.kind, CASE WHEN c.status=200 THEN 'completed' ELSE 'rejected' END
		FROM platform_manager.commands c JOIN platform_manager.installations i ON i.id=c.episode_id
		WHERE c.id=$1 AND c.subject_id=$2`, id, subject).Scan(&receipt.ID, &receipt.Kind, &receipt.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return receipt, errManagerNotFound
	}
	return receipt, err
}
