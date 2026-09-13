package admincommand

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
)

type Store struct {
	pool          *pgxpool.Pool
	integrations  *integrations.Store
	jobs          *jobs.Store
	timeout       time.Duration
	publicBaseURL string
}

func NewStore(pool *pgxpool.Pool, cipher integrations.Cipher, timeout time.Duration, publicBaseURL ...string) *Store {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	base := ""
	if len(publicBaseURL) > 0 {
		base = publicBaseURL[0]
	}
	return &Store{pool: pool, integrations: integrations.NewStore(pool, cipher), jobs: jobs.NewStore(pool), timeout: timeout, publicBaseURL: base}
}

func (s *Store) Execute(ctx context.Context, actor, key string, request Request) (Receipt, error) {
	req, p, keyHash, requestHash, err := normalize(request, key, actor)
	if err != nil {
		return Receipt{}, err
	}
	defer clear(req.Payload)
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "admin-key:"+hex.EncodeToString(keyHash[:])); err != nil {
		return Receipt{}, err
	}
	existing, oldHash, err := loadReceipt(ctx, tx, `c.key_hash=$1`, keyHash[:])
	if err == nil {
		if !bytes.Equal(oldHash, requestHash[:]) {
			return Receipt{}, conflict("idempotency key was used with a different request")
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	lockTarget := req.TargetID
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "admin-target:"+req.TargetType+":"+lockTarget).Scan(&locked); err != nil {
		return Receipt{}, err
	}
	if !locked {
		return Receipt{}, conflict("another command is executing for this target")
	}
	var busy bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_commands WHERE target_type=$1 AND target_id=$2 AND state IN ('accepted','pending','running'))`, req.TargetType, req.TargetID).Scan(&busy); err != nil {
		return Receipt{}, err
	}
	if busy {
		return Receipt{}, conflict("another command is pending for this target")
	}
	id := ReceiptID(key)
	if _, err = tx.Exec(ctx, `INSERT INTO admin_commands(id,key_hash,request_hash,actor_id,target_type,target_id,command,state)
		VALUES($1,$2,$3,$4,$5,$6,$7,'accepted')`, id, keyHash[:], requestHash[:], actor, req.TargetType, req.TargetID, req.Command); err != nil {
		return Receipt{}, err
	}
	// A savepoint ensures failed domain mutations cannot leave partial SQL state,
	// while their safe terminal receipt and failure audit remain durable.
	domainTx, err := tx.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	result, jobID, installationID, operationErr := s.apply(ctx, domainTx, actor, id, req, p)
	state, outcome := "succeeded", "completed"
	var safeError *Error
	if operationErr != nil {
		if err := domainTx.Rollback(ctx); err != nil {
			return Receipt{}, err
		}
		state, outcome = "failed", "rejected"
		safeError = classify(operationErr)
		result = map[string]any{}
		jobID, installationID = nil, nil
	} else {
		if err := domainTx.Commit(ctx); err != nil {
			return Receipt{}, err
		}
		if req.Command == "check" || req.Command == "uninstall" {
			state, outcome = "pending", ""
		} else if jobID != nil || req.TargetType == "delivery" {
			outcome = "queued"
		}
	}
	encoded, _ := json.Marshal(result)
	var encodedError []byte
	if safeError != nil {
		encodedError, _ = json.Marshal(safeError)
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_commands SET state=$2,outcome=$3,result=$4,error=$5,job_id=$6,installation_id=$7,
		finished_at=CASE WHEN $2='pending' THEN NULL ELSE now() END WHERE id=$1`, id, state, outcome, encoded, nullable(encodedError), jobID, installationID); err != nil {
		return Receipt{}, err
	}
	if err = auditReceipt(ctx, tx, id, actor, req.TargetType, req.TargetID, req.Command, state, installationID); err != nil {
		return Receipt{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Receipt{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) apply(ctx context.Context, tx pgx.Tx, actor string, receiptID uuid.UUID, req Request, p commandPayload) (map[string]any, *uuid.UUID, *uuid.UUID, error) {
	var id uuid.UUID
	if req.TargetID != "new" {
		id, _ = uuid.Parse(req.TargetID)
	}
	var installationID *uuid.UUID
	var code, status, integrationStatus string
	switch req.TargetType {
	case "integration":
		code = p.Code
		if req.Command != "create" {
			if err := tx.QueryRow(ctx, `SELECT code FROM integrations WHERE id=$1`, id).Scan(&code); err != nil {
				return nil, nil, nil, err
			}
		}
	case "installation":
		installationID = &id
		if err := tx.QueryRow(ctx, `SELECT ig.code,i.status,ig.status FROM installations i JOIN integrations ig ON ig.id=i.integration_id WHERE i.id=$1`, id).Scan(&code, &status, &integrationStatus); err != nil {
			return nil, nil, nil, err
		}
	}
	if req.TargetType == "integration" || (req.TargetType == "installation" && (req.Command == "enable" || req.Command == "disable" || req.Command == "revoke" || req.Command == "uninstall")) {
		c := integrationCommand(req, p, actor, code)
		defer clear(c.Secret)
		result, err := s.integrations.ApplyTx(ctx, tx, c)
		if err != nil {
			return nil, nil, installationID, err
		}
		data := map[string]any{"integration_id": result.ID, "code": result.Code, "status": result.Status, "action": result.Action}
		if result.InstallationID != nil {
			data["installation_id"] = result.InstallationID
		}
		if req.Command == "revoke" {
			data["next_step"] = "reauthorize"
			var redirect string
			if err := tx.QueryRow(ctx, `SELECT redirect_uri FROM integrations WHERE id=$1`, result.ID).Scan(&redirect); err != nil {
				return nil, nil, installationID, err
			}
			if s.publicBaseURL != "" {
				redirect = s.publicBaseURL
			}
			parsed, err := url.Parse(redirect)
			if err == nil && parsed.Scheme == "https" && parsed.Host != "" {
				start := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: "/oauth/amocrm/start", RawQuery: url.Values{"integration_code": {result.Code}}.Encode()}
				data["oauth_start_url"] = start.String()
			}
		}
		if req.Command != "uninstall" {
			return data, nil, installationID, nil
		}
		job, err := s.enqueueExternal(ctx, tx, receiptID, id, actor, UninstallJobType)
		if err != nil {
			return nil, nil, installationID, err
		}
		return data, &job.ID, installationID, nil
	}
	if req.TargetType == "installation" {
		if err := tx.QueryRow(ctx, `SELECT status FROM integrations WHERE code=$1 FOR SHARE`, code).Scan(&integrationStatus); err != nil {
			return nil, nil, installationID, err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1 FOR SHARE`, id).Scan(&status); err != nil {
			return nil, nil, installationID, err
		}
		switch req.Command {
		case "check":
			if status == "disabled" || status == "uninstalled" || integrationStatus != "active" {
				return nil, nil, installationID, integrations.ErrInvalidState
			}
			job, err := s.enqueueExternal(ctx, tx, receiptID, id, actor, CheckJobType)
			return map[string]any{}, &job.ID, installationID, err
		case "reconcile":
			job, err := webhook.EnqueueReconcileTx(ctx, tx, s.jobs, id, actor)
			return map[string]any{"job_id": job.ID, "status": "queued"}, &job.ID, installationID, err
		case "pilot-enable", "pilot-disable":
			enabled := req.Command == "pilot-enable"
			if enabled && (status != "active" || integrationStatus != "active") {
				return nil, nil, installationID, integrations.ErrInvalidState
			}
			err := activitybridge.SetPilotTx(ctx, tx, id, enabled, "admin", actor)
			return map[string]any{"installation_id": id, "enabled": enabled}, nil, installationID, err
		}
	}
	if req.TargetType == "job" {
		job, err := s.jobs.RetrySafeTx(ctx, tx, id, actor)
		return map[string]any{"job_id": job.ID, "status": "retry"}, &job.ID, job.InstallationID, err
	}
	if req.TargetType == "delivery" {
		if err := tx.QueryRow(ctx, `SELECT installation_id FROM activity_command_receipts WHERE command_id=$1`, id).Scan(&installationID); err != nil {
			return nil, nil, nil, err
		}
		if installationID == nil || installationID.String() != p.InstallationID {
			return nil, nil, nil, notFound()
		}
		err := activitybridge.RetryDeliveryTx(ctx, tx, id, "admin", actor)
		return map[string]any{"command_id": id, "status": "pending_delivery"}, nil, installationID, err
	}
	return nil, nil, nil, invalid("unsupported command")
}

func (s *Store) enqueueExternal(ctx context.Context, tx pgx.Tx, receiptID, installationID uuid.UUID, actor, kind string) (jobs.Job, error) {
	return s.jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{InstallationID: &installationID, Type: kind, ActorType: "admin", ActorID: actor,
		ResourceType: "admin_command", ResourceID: receiptID.String(), Payload: map[string]string{"receipt_id": receiptID.String()}, MaxAttempts: 1})
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	r, _, err := loadReceipt(ctx, s.pool, `c.id=$1`, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, notFound()
	}
	return r, err
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadReceipt(ctx context.Context, q querier, where string, arg any) (Receipt, []byte, error) {
	var r Receipt
	var hash, errorJSON []byte
	err := q.QueryRow(ctx, `SELECT c.id,c.target_type,c.target_id,c.command,
		CASE WHEN c.state='pending' AND j.status='processing' THEN 'running' ELSE c.state END,
		c.outcome,c.result,c.error,c.updated_at,c.created_at,c.finished_at,c.job_id,c.request_hash
		FROM admin_commands c LEFT JOIN jobs j ON j.id=c.job_id WHERE `+where, arg).Scan(&r.ID, &r.TargetType, &r.TargetID, &r.Command, &r.State, &r.Outcome, &r.Result, &errorJSON, &r.ObservedAt, &r.CreatedAt, &r.FinishedAt, &r.JobID, &hash)
	if err != nil {
		return r, nil, err
	}
	if len(errorJSON) > 0 {
		if err := json.Unmarshal(errorJSON, &r.Error); err != nil {
			return Receipt{}, nil, err
		}
	}
	r.ObservedAt = r.ObservedAt.UTC()
	r.CreatedAt = r.CreatedAt.UTC()
	return r, hash, nil
}

func auditReceipt(ctx context.Context, tx jobs.TxExecutor, id uuid.UUID, actor, targetType, targetID, command, state string, installationID *uuid.UUID) error {
	metadata, _ := json.Marshal(map[string]string{"receipt_id": id.String(), "state": state, "command": command})
	_, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata)
		VALUES($1,'admin',$2,'admin.command.'||$3,$4,$5,$6)`, installationID, actor, state, targetType, targetID, metadata)
	return err
}

func classify(err error) *Error {
	var api *Error
	if errors.As(err, &api) {
		return api
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, integrations.ErrNotFound) || errors.Is(err, jobs.ErrNotFound) {
		return notFound()
	}
	if errors.Is(err, integrations.ErrConflict) || errors.Is(err, integrations.ErrInvalidState) || errors.Is(err, jobs.ErrRetryNotAllowed) || errors.Is(err, webhook.ErrNotActive) {
		return conflict(err.Error())
	}
	switch serviceapi.ErrorCode(err) {
	case serviceapi.NotFound:
		return notFound()
	case serviceapi.Conflict:
		return conflict("command precondition failed")
	}
	return &Error{Code: "internal", Message: "command failed", Status: 500}
}
func nullable(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
