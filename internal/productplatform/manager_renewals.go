package productplatform

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type renewalInput struct {
	Product     uuid.UUID `json:"product_id"`
	TargetUntil time.Time `json:"target_until"`
	Reason      string    `json:"reason"`
}

type accountRenewal struct {
	ID          uuid.UUID   `json:"id"`
	Account     uuid.UUID   `json:"account_id"`
	Product     uuid.UUID   `json:"product_id"`
	TargetUntil time.Time   `json:"target_until"`
	Reason      string      `json:"reason"`
	Access      accessState `json:"access"`
}

type renewalTerm struct {
	Until   *time.Time `json:"valid_until"`
	Source  string     `json:"source"`
	Version int64      `json:"entitlement_version"`
}

type renewalEvidence struct {
	Installation uuid.UUID    `json:"core_installation_id"`
	Generation   int64        `json:"generation"`
	Before       *renewalTerm `json:"before"`
	After        renewalTerm  `json:"after"`
}

func parseRenewalInput(raw []byte) (renewalInput, error) {
	var input renewalInput
	var product, target string
	if err := readManagerObject(raw, map[string]any{"product_id": &product, "target_until": &target, "reason": &input.Reason}); err != nil {
		return renewalInput{}, err
	}
	id, idErr := managerUUID(product)
	until, dateErr := time.Parse(time.RFC3339Nano, target)
	until = until.UTC().Truncate(time.Microsecond)
	input.Reason = strings.TrimSpace(input.Reason)
	if idErr != nil || dateErr != nil || strings.Contains(target, ",") || until.Year() < 1 || until.Year() > 9999 ||
		!managerText(input.Reason, 1, 1000) {
		return renewalInput{}, errManagerInput
	}
	input.Product, input.TargetUntil = id, until
	return input, nil
}

func (m managerHTTP) createAccountRenewal(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	key, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, versionErr := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if idErr != nil || keyErr != nil || versionErr != nil || contentType != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	input, err := parseRenewalInput(raw)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	cmd := managerCommand{ID: env.Command, Key: key, Subject: env.Actor.Subject, RequestID: env.RequestID,
		Account: id, Product: input.Product, Kind: renewalCommandKind, Version: version}
	outcome, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return renewAccountAccess(ctx, tx, cmd, input)
	})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}

func renewAccountAccess(ctx context.Context, tx pgx.Tx, cmd managerCommand, input renewalInput) (managerOutcome, error) {
	var installation uuid.UUID
	err := tx.QueryRow(ctx, `SELECT installation_id FROM platform_manager.account_memberships
		WHERE account_id=$1 AND product_id=$2`, cmd.Account, cmd.Product).Scan(&installation)
	if err != nil {
		return managerOutcome{}, accountReadError(err)
	}
	stream, err := lockStream(ctx, tx, installation)
	if err != nil {
		return managerOutcome{}, accountReadError(err)
	}
	var current bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.installations
		WHERE account_id=$1 AND product_id=$2 AND installation_id=$3 AND generation=$4 AND lifecycle='current')`,
		cmd.Account, cmd.Product, installation, stream.generation).Scan(&current)
	if err != nil {
		return managerOutcome{}, err
	}
	if !current {
		return rejectRenewal(cmd, "renewal_unavailable")
	}

	previous, err := lockRenewalTerm(ctx, tx, cmd)
	if err != nil {
		return managerOutcome{}, err
	}
	version := int64(1)
	if previous != nil {
		version = previous.Version
	}
	if version != cmd.Version {
		return managerVersionConflict(cmd, version)
	}
	if version == wire.MaxPosition || previous != nil && !previous.renewable() {
		return rejectRenewal(cmd, "renewal_unavailable")
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&now); err != nil {
		return managerOutcome{}, err
	}
	if !input.TargetUntil.After(now) || previous != nil && !input.TargetUntil.After(*previous.Until) {
		return rejectRenewal(cmd, "target_not_extended")
	}
	updated := renewalTerm{Until: &input.TargetUntil, Source: "manual", Version: version + 1}
	if previous == nil {
		_, err = tx.Exec(ctx, `INSERT INTO platform_lifecycle.terms(account_id,product_id,valid_until,source,reason,entitlement_version)
			VALUES($1,$2,$3,$4,$5,$6)`, cmd.Account, cmd.Product, updated.Until, updated.Source, input.Reason, updated.Version)
	} else {
		_, err = tx.Exec(ctx, `UPDATE platform_lifecycle.terms SET valid_until=$3,source=$4,reason=$5,entitlement_version=$6
			WHERE account_id=$1 AND product_id=$2`, cmd.Account, cmd.Product, updated.Until, updated.Source, input.Reason, updated.Version)
	}
	if err != nil {
		return managerOutcome{}, err
	}
	event, err := recordAccountChange(ctx, tx, cmd, accountChange{
		Account: cmd.Account, Product: cmd.Product, Kind: accountAccessRenewed, Version: updated.Version, Reason: &input.Reason,
		Details: renewalEvidence{Installation: installation, Generation: stream.generation, Before: previous, After: updated},
	})
	if err != nil {
		return managerOutcome{}, err
	}
	access := accessProjection{account: cmd.Account, product: cmd.Product, until: updated.Until, source: &updated.Source, version: updated.Version}
	if err := tx.QueryRow(ctx, `SELECT (SELECT package_until FROM platform_manager.subscription_terms WHERE account_id=$1 AND product_id=$2)`, cmd.Account, cmd.Product).Scan(&access.packageUntil); err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusCreated, updated.Version, accountRenewal{
		ID: event, Account: cmd.Account, Product: cmd.Product, TargetUntil: input.TargetUntil, Reason: input.Reason, Access: access.state(now),
	})
}

func lockRenewalTerm(ctx context.Context, tx pgx.Tx, cmd managerCommand) (*renewalTerm, error) {
	var current renewalTerm
	err := tx.QueryRow(ctx, `SELECT valid_until,source,entitlement_version FROM platform_lifecycle.terms
		WHERE account_id=$1 AND product_id=$2 FOR UPDATE`, cmd.Account, cmd.Product).
		Scan(&current.Until, &current.Source, &current.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if current.Until != nil {
		until := current.Until.UTC()
		current.Until = &until
	}
	return &current, err
}

func (t renewalTerm) renewable() bool {
	switch t.Source {
	case "own_trial", "free", "own_paid", "manual":
		return t.Until != nil
	default:
		return false
	}
}

func rejectRenewal(cmd managerCommand, code string) (managerOutcome, error) {
	return managerMutationResponse(http.StatusUnprocessableEntity, 0,
		managerProblem(http.StatusUnprocessableEntity, code, cmd.RequestID, 0, 0))
}
