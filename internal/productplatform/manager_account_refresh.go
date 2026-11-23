package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

const accountRefreshKind = "account.refresh"

var errAccountGatewayDisabled = errors.New("account Gateway is disabled")

type accountComponent string

const (
	refreshLicense       accountComponent = "license"
	refreshUsers         accountComponent = "users"
	refreshInstallations accountComponent = "installations"
)

func (c accountComponent) valid() bool {
	return c == refreshLicense || c == refreshUsers
}

type accountRefreshInput struct {
	Components []accountComponent `json:"components"`
	Reason     string             `json:"reason"`
}

func (i *accountRefreshInput) normalize() bool {
	i.Reason = strings.TrimSpace(i.Reason)
	if !managerText(i.Reason, 1, 1000) || len(i.Components) == 0 || len(i.Components) > 2 {
		return false
	}
	slices.Sort(i.Components)
	return i.Components[0].valid() && (len(i.Components) == 1 || i.Components[1].valid() && i.Components[0] != i.Components[1])
}

type accountRefreshCommand struct {
	ID, Key, Account, Subject uuid.UUID
	Input                     accountRefreshInput
}

type accountOperation struct {
	ID         uuid.UUID              `json:"id"`
	Kind       string                 `json:"kind"`
	State      string                 `json:"state"`
	Target     accountOperationTarget `json:"target"`
	CreatedAt  time.Time              `json:"created_at"`
	UpdatedAt  time.Time              `json:"updated_at"`
	Error      *managerErrorResponse  `json:"error,omitempty"`
	RetryAfter int                    `json:"retry_after_seconds,omitempty"`
}

type accountOperationTarget struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

func (s managerStore) refreshAccount(ctx context.Context, cmd accountRefreshCommand, enabled bool) ([]byte, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT transaction_timestamp() FROM platform_manager.visible_accounts WHERE account_id=$1`, cmd.Account).Scan(&at)
	if err != nil {
		return nil, accountReadError(err)
	}
	body, _ := json.Marshal(cmd.Input)
	hash := sha256.Sum256(body)
	if !enabled {
		return replayRefresh(ctx, tx, cmd, hash, errAccountGatewayDisabled)
	}
	at = at.UTC()
	op := accountOperation{ID: cmd.ID, Kind: accountRefreshKind, State: "accepted",
		Target: accountOperationTarget{Kind: "account", ID: cmd.Account}, CreatedAt: at, UpdatedAt: at}
	response, _ := json.Marshal(op)
	reserved, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.commands (id,subject_id,idempotency_key,kind,account_id,body_hash,status,response)
		VALUES($1,$2,$3,'account.refresh',$4,$5,202,$6) ON CONFLICT DO NOTHING`,
		cmd.ID, cmd.Subject, cmd.Key, cmd.Account, hash[:], response)
	if err != nil {
		return nil, err
	}
	if reserved.RowsAffected() == 0 {
		return replayRefresh(ctx, tx, cmd, hash, errManagerConflict)
	}
	scope, err := accountSourceScope(ctx, tx, cmd.Account)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	for _, part := range cmd.Input.Components {
		if err := lockRefreshSlot(ctx, tx, cmd.Account, part); err != nil {
			return nil, err
		}
		job, err := joinAccountRefresh(ctx, tx, cmd.Account, part, scope)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform_manager.account_refresh_work(command_id,component,job_id) VALUES($1,$2,$3)`,
			cmd.ID, part, job); err != nil {
			return nil, err
		}
	}
	return response, tx.Commit(ctx)
}

func replayRefresh(ctx context.Context, tx pgx.Tx, cmd accountRefreshCommand, hash [32]byte, missing error) ([]byte, error) {
	var response []byte
	err := tx.QueryRow(ctx, `
		SELECT c.response FROM platform_manager.commands c
		JOIN platform_manager.visible_accounts a ON a.account_id=c.account_id
		WHERE c.id=$1 AND c.subject_id=$2 AND c.idempotency_key=$3 AND c.account_id=$4 AND c.body_hash=$5
		  AND c.kind='account.refresh' AND c.status=202`, cmd.ID, cmd.Subject, cmd.Key, cmd.Account, hash[:]).
		Scan(&response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	return response, tx.Commit(ctx)
}

func lockRefreshSlot(ctx context.Context, tx pgx.Tx, account uuid.UUID, part accountComponent) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.account_refresh_slots(account_id,component)
		VALUES($1,$2) ON CONFLICT DO NOTHING`, account, part)
	if err != nil {
		return err
	}
	var id uuid.UUID
	return tx.QueryRow(ctx, `
		SELECT account_id FROM platform_manager.account_refresh_slots
		WHERE account_id=$1 AND component=$2 FOR UPDATE`, account, part).Scan(&id)
}

func nullableSeed(scope sourceScope) *uuid.UUID {
	if scope.Installation == uuid.Nil || scope.ObservationOnly {
		return nil
	}
	return &scope.Installation
}

func joinAccountRefresh(ctx context.Context, tx pgx.Tx, account uuid.UUID, part accountComponent, scope sourceScope) (uuid.UUID, error) {
	var current *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT j.id FROM platform_manager.account_refresh_slots s LEFT JOIN platform_jobs.jobs j
			ON j.id=s.job_id AND j.status IN ('queued','running')
		WHERE s.account_id=$1 AND s.component=$2`, account, part).Scan(&current)
	if err != nil {
		return uuid.Nil, err
	}
	if current != nil {
		return *current, nil
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx, `SELECT platform_manager.enqueue_account_refresh($1,$2,$3)`, account, part, id); err != nil {
		return uuid.Nil, err
	}
	var installation *uuid.UUID
	var generation, sequence *int64
	if nullableSeed(scope) != nil {
		installation, generation, sequence = &scope.Installation, &scope.Generation, &scope.AuthorizationSequence
	}
	_, err = tx.Exec(ctx, `
		UPDATE platform_manager.account_refresh_slots SET job_id=$3,
      next_check_at=date_bin(interval '1 day',statement_timestamp(),
        timestamptz '2000-01-01 00:00:00+00' + mod(abs(hashtext(account_id::text)::bigint),86400)*interval '1 second') + interval '1 day',
			seed_installation_id=$4,seed_generation=$5,seed_authorization_sequence=$6
		WHERE account_id=$1 AND component=$2`, account, part, id, installation, generation, sequence)
	return id, err
}

type accountWorkState struct {
	Status      queue.Status
	AvailableAt time.Time
	UpdatedAt   time.Time
	Failure     *string
}

func (s managerStore) accountOperation(ctx context.Context, id, actor, requestID uuid.UUID) (accountOperation, error) {
	var operation accountOperation
	rows, err := s.pool.Query(ctx, `
		SELECT cmd.account_id,cmd.created_at,j.status,j.available_at,j.updated_at,j.failure_code,statement_timestamp()
		FROM platform_manager.commands cmd
		JOIN platform_manager.visible_accounts a ON a.account_id=cmd.account_id
		JOIN platform_manager.account_refresh_work w ON w.command_id=cmd.id
		JOIN platform_jobs.jobs j ON j.id=w.job_id
		WHERE cmd.id=$1 AND cmd.subject_id=$2 AND cmd.kind='account.refresh' AND cmd.status=202
		ORDER BY w.component`, id, actor)
	if err != nil {
		return operation, err
	}
	defer rows.Close()
	work := make([]accountWorkState, 0, 2)
	var account uuid.UUID
	var created, now time.Time
	for rows.Next() {
		var state accountWorkState
		if err := rows.Scan(&account, &created, &state.Status, &state.AvailableAt, &state.UpdatedAt, &state.Failure, &now); err != nil {
			return operation, err
		}
		work = append(work, state)
	}
	if err := rows.Err(); err != nil {
		return operation, err
	}
	if len(work) == 0 {
		return operation, errManagerNotFound
	}
	return summarizeAccountOperation(id, account, requestID, created, now, work), nil
}

func summarizeAccountOperation(id, account, requestID uuid.UUID, created, now time.Time, work []accountWorkState) accountOperation {
	op := accountOperation{ID: id, Kind: accountRefreshKind, Target: accountOperationTarget{Kind: "account", ID: account},
		CreatedAt: created.UTC(), UpdatedAt: created.UTC()}
	var active, pending, delayed bool
	var failure string
	for _, state := range work {
		if state.UpdatedAt.After(op.UpdatedAt) {
			op.UpdatedAt = state.UpdatedAt.UTC()
		}
		switch state.Status {
		case queue.Running:
			active = true
		case queue.Queued:
			pending = true
			if state.AvailableAt.After(now) {
				delayed = true
				delay := state.AvailableAt.Sub(now)
				seconds := int(delay / time.Second)
				if delay%time.Second != 0 {
					seconds++
				}
				if op.RetryAfter == 0 || op.RetryAfter > seconds {
					op.RetryAfter = seconds
				}
			}
		case queue.Dead:
			if failure == "" {
				failure = "source_unavailable"
				if state.Failure != nil {
					failure = *state.Failure
				}
			}
		}
	}
	switch {
	case active:
		op.State, op.RetryAfter = "running", 0
	case delayed:
		op.State = "waiting_external"
	case pending:
		op.State = "accepted"
	case failure != "":
		op.State = "failed"
		problem := managerProblem(http.StatusServiceUnavailable, failure, requestID, 0, 0)
		op.Error = &problem
	default:
		op.State = "succeeded"
	}
	return op
}
