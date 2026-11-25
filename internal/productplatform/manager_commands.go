package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	profileCommandKind             = "account.profile"
	portraitCommandKind            = "account.portrait"
	portraitAuditCommandKind       = "account.portrait_audit_date"
	subscriptionCommandKind        = "account.subscription"
	renewalCommandKind             = "account.renewal"
	portraitTemplateCommandKind    = "portrait.template"
	portraitNicheCreateCommandKind = "portrait.niche.create"
	portraitNicheUpdateCommandKind = "portrait.niche.update"
	portraitNicheDeleteCommandKind = "portrait.niche.delete"
	portraitNicheMergeCommandKind  = "portrait.niche.merge"
)

var errManagerPermission = errors.New("manager permission denied")

type managerCommand struct {
	ID, Key, Subject, RequestID, Account, Product, Resource uuid.UUID
	Kind                                                    string
	Version                                                 int64
}

func (s managerStore) mutate(ctx context.Context, cmd managerCommand, input any,
	apply func(context.Context, pgx.Tx) (managerOutcome, error)) (managerOutcome, error) {
	var outcome managerOutcome
	normalized, err := json.Marshal(input)
	if err != nil {
		return outcome, err
	}
	hash := sha256.Sum256(normalized)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return outcome, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	account, product, resource := commandUUID(cmd.Account), commandUUID(cmd.Product), commandUUID(cmd.Resource)
	if err := requireCommandScope(ctx, tx, cmd.Kind, nil, account, product); err != nil {
		return outcome, err
	}

	var version *int64
	if cmd.Version != 0 {
		version = &cmd.Version
	}
	reserved, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.commands
			(id,subject_id,idempotency_key,kind,account_id,product_id,resource_id,expected_version,body_hash)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`,
		cmd.ID, cmd.Subject, cmd.Key, cmd.Kind, account, product, resource, version, hash[:])
	if err != nil {
		return outcome, err
	}
	if reserved.RowsAffected() == 0 {
		if err := requireCommandScope(ctx, tx, cmd.Kind, nil, account, product); err != nil {
			return outcome, err
		}
		err := tx.QueryRow(ctx, `
			SELECT status,response,etag,resource_id FROM platform_manager.commands
			WHERE id=$1 AND subject_id=$2 AND idempotency_key=$3 AND kind=$4
			  AND account_id IS NOT DISTINCT FROM $5 AND product_id IS NOT DISTINCT FROM $6
			  AND ($4='product.create' OR resource_id IS NOT DISTINCT FROM $7)
			  AND expected_version IS NOT DISTINCT FROM $8 AND body_hash=$9`,
			cmd.ID, cmd.Subject, cmd.Key, cmd.Kind, account, product, resource, version, hash[:]).
			Scan(&outcome.status, &outcome.body, &outcome.etag, &resource)
		if errors.Is(err, pgx.ErrNoRows) {
			return outcome, errManagerConflict
		}
		if err != nil {
			return outcome, err
		}
		if cmd.Kind == productCreateCommandKind && outcome.status == http.StatusCreated {
			if err := requireCommandScope(ctx, tx, productUpdateCommandKind, nil, nil, resource); err != nil {
				return outcome, err
			}
		}
		return outcome, tx.Commit(ctx)
	}

	outcome, err = apply(ctx, tx)
	if err != nil {
		return outcome, err
	}
	if err := requireCommandScope(ctx, tx, cmd.Kind, nil, account, product); err != nil {
		return outcome, err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_manager.commands SET status=$2,response=$3,etag=$4 WHERE id=$1`,
		cmd.ID, outcome.status, outcome.body, outcome.etag); err != nil {
		return outcome, err
	}
	return outcome, tx.Commit(ctx)
}

func commandUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func managerMutationResponse(status int, version int64, body any) (managerOutcome, error) {
	outcome := managerOutcome{status: status}
	if version != 0 {
		etag := managerETag(version)
		outcome.etag = &etag
	}
	if body == nil && status == http.StatusNoContent {
		return outcome, nil
	}
	var err error
	outcome.body, err = json.Marshal(body)
	return outcome, err
}

func managerVersionConflict(cmd managerCommand, current int64) (managerOutcome, error) {
	return managerMutationResponse(http.StatusConflict, 0,
		managerProblem(http.StatusConflict, "version_conflict", cmd.RequestID, current, 0))
}

func managerETag(version int64) string {
	return strconv.Quote(strconv.FormatInt(version, 10))
}

func writeManagerOutcome(w http.ResponseWriter, outcome managerOutcome) {
	w.Header().Set("Cache-Control", "no-store")
	if outcome.status != http.StatusNoContent {
		w.Header().Set("Content-Type", "application/json")
	}
	if outcome.etag != nil {
		w.Header().Set("ETag", *outcome.etag)
	}
	w.WriteHeader(outcome.status)
	if len(outcome.body) != 0 {
		_, _ = w.Write(outcome.body)
	}
}

type managerReceipt struct {
	ID          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	State       string     `json:"state"`
	OperationID *uuid.UUID `json:"operation_id,omitempty"`
}

func (s managerStore) receipt(ctx context.Context, id uuid.UUID, actor managerActor) (managerReceipt, error) {
	var receipt managerReceipt
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return receipt, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var episode, account, product, resource *uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id,kind,
			CASE status WHEN 202 THEN 'accepted' WHEN 409 THEN 'rejected' WHEN 422 THEN 'rejected' ELSE 'completed' END,
			CASE WHEN status=202 THEN id END,episode_id,account_id,product_id,resource_id
		FROM platform_manager.commands WHERE id=$1 AND subject_id=$2`, id, actor.Subject).
		Scan(&receipt.ID, &receipt.Kind, &receipt.State, &receipt.OperationID, &episode, &account, &product, &resource)
	if err != nil {
		return receipt, accountReadError(err)
	}
	grant := commandGrant(receipt.Kind)
	if grant == "" {
		return receipt, errManagerNotFound
	}
	if receipt.Kind == productCreateCommandKind {
		product = resource
		if err := requireCommandScope(ctx, tx, productUpdateCommandKind, nil, nil, product); err != nil && receipt.State != "rejected" {
			return receipt, err
		}
	}
	if err := requireCommandScope(ctx, tx, receipt.Kind, episode, account, product); err != nil {
		return receipt, err
	}
	if !actor.Grants[grant] {
		return receipt, errManagerPermission
	}
	return receipt, tx.Commit(ctx)
}

func commandGrant(kind string) string {
	switch kind {
	case reviewCommandKind, accountRefreshKind, profileCommandKind, portraitCommandKind, portraitAuditCommandKind:
		return "clients.write"
	case renewalCommandKind, subscriptionCommandKind:
		return "clients.renew"
	case productCreateCommandKind, productUpdateCommandKind, companyWorkflowCommandKind, installationDealSettingsCommandKind:
		return "catalog.manage"
	case portraitTemplateCommandKind, portraitNicheCreateCommandKind, portraitNicheUpdateCommandKind,
		portraitNicheDeleteCommandKind, portraitNicheMergeCommandKind:
		return "configuration.manage"
	default:
		return ""
	}
}

func requireCommandScope(ctx context.Context, tx pgx.Tx, kind string, episode, account, product *uuid.UUID) error {
	var visible bool
	var err error
	switch kind {
	case reviewCommandKind:
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.installations WHERE id=$1)`, episode).Scan(&visible)
	case subscriptionCommandKind, accountRefreshKind, profileCommandKind, portraitCommandKind, portraitAuditCommandKind:
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.visible_accounts WHERE account_id=$1)`, account).Scan(&visible)
	case productCreateCommandKind, installationDealSettingsCommandKind:
		return nil
	case productUpdateCommandKind, companyWorkflowCommandKind:
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_lifecycle.products WHERE id=$1 AND catalog_version IS NOT NULL)`, product).Scan(&visible)
	case renewalCommandKind:
		err = tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT FROM platform_manager.account_memberships WHERE account_id=$1 AND product_id=$2)`,
			account, product).Scan(&visible)
	case portraitTemplateCommandKind, portraitNicheCreateCommandKind, portraitNicheUpdateCommandKind,
		portraitNicheDeleteCommandKind, portraitNicheMergeCommandKind:
		return nil
	}
	if err != nil {
		return err
	}
	if !visible {
		return errManagerNotFound
	}
	return nil
}
