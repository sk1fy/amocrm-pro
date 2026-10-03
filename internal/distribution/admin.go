package distribution

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/services"
)

// AdminCommand is a strictly validated owner command. It cannot change rules,
// recipients, frozen envelopes or evidence of an ambiguous assignment.
type AdminCommand struct {
	Kind, MessageID, OperationID string
	ExpectedAttempts             *int
	ExpectedPaused               *bool
	ExpectedResultVersion        *int64
}

func AdminApplyTx(ctx context.Context, tx pgx.Tx, installation uuid.UUID, command string, in AdminCommand) (map[string]any, error) {
	switch command {
	case "distribution-pause", "distribution-resume":
		var paused bool
		if e := tx.QueryRow(ctx, `SELECT paused FROM distribution_admin_pauses WHERE installation_id=$1 FOR UPDATE`, installation).Scan(&paused); e != nil {
			return nil, e
		}
		if in.ExpectedPaused == nil || paused != *in.ExpectedPaused {
			return nil, ErrConflict
		}
		target := command == "distribution-pause"
		if _, e := tx.Exec(ctx, `UPDATE distribution_admin_pauses SET paused=$2 WHERE installation_id=$1`, installation, target); e != nil {
			return nil, e
		}
		return map[string]any{"installation_id": installation, "paused": target, "scope": "admission_and_new_assignment_dispatch", "team_rules": "unchanged", "existing_effects": "preserved"}, nil
	case "distribution-delivery-retry":
		if e := services.RequireEnabled(ctx, tx, installation, services.LeadDistribution, true); e != nil {
			return nil, ErrDenied
		}
		table, predicate := "distribution_event_outbox", "scope->>'installationId'=$1::text AND message_id::text=$2"
		if in.Kind == "results" {
			table = "distribution_result_outbox"
			predicate = "operation_id IN(SELECT id FROM distribution_operations WHERE installation_id=$1) AND payload->>'messageId'=$2"
		}
		tag, e := tx.Exec(ctx, `UPDATE `+table+` SET state='pending',attempts=0,error_code=NULL,next_attempt_at=clock_timestamp(),lease_token=NULL,lease_until=NULL WHERE `+predicate+` AND state='blocked' AND attempts=$3`, installation, in.MessageID, in.ExpectedAttempts)
		if e != nil {
			return nil, e
		}
		if tag.RowsAffected() != 1 {
			return nil, ErrConflict
		}
		return map[string]any{"message_id": in.MessageID, "delivery_state": "pending", "outcome": "queued", "kind": in.Kind, "previous_attempts": *in.ExpectedAttempts}, nil
	case "distribution-reconcile":
		var version int64
		var finished bool
		var attempted bool
		e := tx.QueryRow(ctx, `SELECT result_version,finished_at IS NOT NULL,EXISTS(SELECT 1 FROM distribution_operation_attempts a WHERE a.operation_id=o.id) FROM distribution_operations o WHERE id=$1 AND installation_id=$2 FOR SHARE`, in.OperationID, installation).Scan(&version, &finished, &attempted)
		if e == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		if e != nil {
			return nil, e
		}
		if in.ExpectedResultVersion == nil || version != *in.ExpectedResultVersion || finished || !attempted {
			return nil, ErrConflict
		}
		return map[string]any{"operation_id": in.OperationID, "assignment_state": "unverified", "outcome": "queued"}, nil
	}
	return nil, ErrDenied
}

// AdminReconcile reads CRM once and then uses the same owner evidence rules as
// the signed operation API. It never calls PATCH and remains unknown without ACK.
func (w *AssignmentWorker) AdminReconcile(ctx context.Context, installation, operation, receipt uuid.UUID, version int64, actor string) (Operation, error) {
	var company uuid.UUID
	if e := w.Store.pool.QueryRow(ctx, `SELECT company_id FROM distribution_operations WHERE id=$1 AND installation_id=$2`, operation, installation).Scan(&company); e == pgx.ErrNoRows {
		return Operation{}, ErrNotFound
	} else if e != nil {
		return Operation{}, e
	}
	identity := Scope{CompanyID: company, InstallationID: installation, KeyID: actor}
	op, e := w.Store.Operation(ctx, identity, operation)
	if e != nil {
		return op, e
	}
	operator := uuid.NewSHA1(uuid.NameSpaceOID, []byte(actor))
	body := operationAction{ExpectedResultVersion: version, Actor: Actor{Kind: "operator", OperatorID: &operator}, Reason: "admin verification of existing operation"}
	replay, found, e := w.Store.ActionReplay(ctx, identity, operation, "reconcile", receipt.String(), body)
	if e != nil || found {
		return replay, e
	}
	if op.ResultVersion != version || op.FinishedAt != nil {
		return op, ErrConflict
	}
	snapshot, e := w.Observe(ctx, op)
	if e != nil {
		return op, e
	}
	return w.Store.ReconcileAction(ctx, identity, operation, body, receipt.String(), &snapshot)
}

// requireAdmission runs only for new commands/business turns/dispatch. Source
// normalization, read access, deliveries and existing-effect verification ignore
// this pause so already accepted work is preserved.
func requireAdmission(ctx context.Context, db services.Querier, installation uuid.UUID, lock bool) error {
	query := `SELECT paused FROM distribution_admin_pauses WHERE installation_id=$1`
	if lock {
		query += ` FOR SHARE`
	}
	var paused bool
	if e := db.QueryRow(ctx, query, installation).Scan(&paused); e != nil {
		return e
	}
	if paused {
		return ErrDenied
	}
	return nil
}
