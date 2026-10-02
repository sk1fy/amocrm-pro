package distribution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type actionInfo struct {
	Action               string
	KeyHash, PayloadHash [32]byte
	Body                 operationAction
}

func newActionInfo(action, key string, body operationAction, id uuid.UUID) (actionInfo, error) {
	keyID, e := uuid.Parse(key)
	if e != nil || keyID == uuid.Nil || keyID.String() != key {
		return actionInfo{}, ErrDenied
	}
	raw, e := json.Marshal(struct {
		OperationID uuid.UUID       `json:"operationId"`
		Body        operationAction `json:"body"`
	}{id, body})
	if e != nil {
		return actionInfo{}, e
	}
	return actionInfo{action, sha256.Sum256([]byte(key)), sha256.Sum256(raw), body}, nil
}
func readActionReceipt(ctx context.Context, tx opReader, id uuid.UUID, info actionInfo) (Operation, bool, error) {
	var hash, raw []byte
	e := tx.QueryRow(ctx, `SELECT request_hash,response FROM distribution_control_receipts WHERE binding_id=(SELECT binding_id FROM distribution_operations WHERE id=$1) AND action=$2 AND key_hash=$3`, id, info.Action, info.KeyHash[:]).Scan(&hash, &raw)
	if errors.Is(e, pgx.ErrNoRows) {
		return Operation{}, false, nil
	}
	if e != nil {
		return Operation{}, false, e
	}
	if !equalBytes(hash, info.PayloadHash[:]) {
		return Operation{}, false, ErrIdempotencyConflict
	}
	var op Operation
	e = json.Unmarshal(raw, &op)
	return op, true, e
}
func saveActionReceipt(ctx context.Context, tx pgx.Tx, op Operation, scope Scope, info actionInfo) error {
	raw, e := json.Marshal(op)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO distribution_control_receipts(binding_id,operation_id,action,key_hash,request_hash,response) VALUES($1,$2,$3,$4,$5,$6)`, op.Scope.BindingID, op.OperationID, info.Action, info.KeyHash[:], info.PayloadHash[:], raw); e != nil {
		var pe *pgconn.PgError
		if errors.As(e, &pe) && pe.Code == "23505" {
			return ErrIdempotencyConflict
		}
		return e
	}
	actor, _ := json.Marshal(info.Body.Actor)
	_, e = tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'service',$2,$3,'distribution_operation',$4,jsonb_build_object('actor',$5::jsonb,'reason',$6::text,'result_version',$7::bigint))`, op.Scope.InstallationID, scope.KeyID, "distribution."+info.Action, op.OperationID.String(), actor, info.Body.Reason, op.ResultVersion)
	return e
}
func (s *Store) ActionReplay(ctx context.Context, scope Scope, id uuid.UUID, action, key string, body operationAction) (Operation, bool, error) {
	if _, e := s.Operation(ctx, scope, id); e != nil {
		return Operation{}, false, e
	}
	info, e := newActionInfo(action, key, body, id)
	if e != nil {
		return Operation{}, false, e
	}
	return readActionReceipt(ctx, s.pool, id, info)
}
