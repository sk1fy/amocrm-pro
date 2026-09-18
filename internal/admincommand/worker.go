package admincommand

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/oauth"
	"github.com/sk1fy/amocrm-pro/internal/platform/cryptox"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
)

type WorkerExecutor struct {
	pool       *pgxpool.Pool
	Check      func(context.Context, uuid.UUID) error
	Unregister func(context.Context, uuid.UUID) error
}

func NewWorkerExecutor(pool *pgxpool.Pool, client *amocrm.Client, keys *cryptox.KeyRing, gateway oauth.OAuthGateway) *WorkerExecutor {
	return &WorkerExecutor{pool: pool,
		Check: func(ctx context.Context, id uuid.UUID) error {
			var account struct {
				ID int64 `json:"id"`
			}
			if err := client.DoJSON(ctx, id, http.MethodGet, "/api/v4/account", nil, &account); err != nil {
				return err
			}
			var expected int64
			if err := pool.QueryRow(ctx, `SELECT account_id FROM installations WHERE id=$1`, id).Scan(&expected); err != nil {
				return err
			}
			if account.ID != expected {
				return amocrm.ErrIncompleteResponse
			}
			return nil
		},
		Unregister: func(ctx context.Context, id uuid.UUID) error {
			tokens, err := oauth.NewUninstallTokenProvider(pool, keys, gateway, id)
			if err != nil {
				return err
			}
			return webhook.Unregister(ctx, webhook.NewReconcileStore(pool, keys), client.WithTokenProvider(tokens), id)
		},
	}
}

type executionResult struct {
	State   string         `json:"state"`
	Outcome string         `json:"outcome"`
	Result  map[string]any `json:"result"`
	Error   *Error         `json:"error,omitempty"`
}

func (w *WorkerExecutor) Handler(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
	if job.InstallationID == nil || job.ActorType == nil || *job.ActorType != "admin" || job.ResourceID == nil || job.ResourceType == nil || *job.ResourceType != "admin_command" {
		return nil, jobs.Permanent("invalid_admin_job", errors.New("invalid admin job scope"))
	}
	var command, state string
	var receiptID uuid.UUID
	err := w.pool.QueryRow(ctx, `SELECT id,command,state FROM admin_commands WHERE job_id=$1 AND installation_id=$2 AND id::text=$3`, job.ID, job.InstallationID, *job.ResourceID).Scan(&receiptID, &command, &state)
	if err != nil {
		return nil, jobs.Permanent("invalid_admin_job", errors.New("admin command receipt not found"))
	}
	if state != "pending" && state != "running" {
		return nil, jobs.Permanent("invalid_admin_job", errors.New("admin command is already terminal"))
	}
	result := executionResult{State: "succeeded", Outcome: "completed", Result: map[string]any{}}
	switch {
	case command == "check" && job.Type == CheckJobType:
		err := w.Check(ctx, *job.InstallationID)
		classification, retryAfter := classifyCheck(err)
		observed := time.Now().UTC()
		result.Outcome = classification
		result.Result = map[string]any{"classification": classification, "verification": classification, "observed_at": observed, "retry_after": retryAfter}
	case command == "uninstall" && job.Type == UninstallJobType:
		result.Result = map[string]any{"installation_id": job.InstallationID, "status": "uninstalled"}
		if err := w.Unregister(ctx, *job.InstallationID); err != nil {
			result.State, result.Outcome = "partial", "webhook_error"
			message := "remote webhook unregister failed; inspect the installation before retrying"
			if errors.Is(err, oauth.ErrRefreshOutcomeUnknown) {
				message = "OAuth refresh outcome unknown; recover the pending refresh or reauthorize"
			}
			result.Result["webhook_error"] = message
			result.Error = &Error{Code: "webhook_error", Message: message}
		}
	default:
		return nil, jobs.Permanent("invalid_admin_job", errors.New("admin job command mismatch"))
	}
	return json.Marshal(result)
}

func classifyCheck(err error) (string, int64) {
	if err == nil {
		return "verified_ok", 0
	}
	var upstream *amocrm.APIError
	if errors.As(err, &upstream) {
		switch upstream.Kind {
		case amocrm.ErrorUnauthorized, amocrm.ErrorForbidden:
			return "auth_error", 0
		case amocrm.ErrorRateLimited, amocrm.ErrorOverloaded:
			return "rate_limited", int64(math.Ceil(upstream.RetryAfter.Seconds()))
		case amocrm.ErrorTemporary:
			return "network_error", 0
		default:
			return "internal_error", 0
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "auth_error", 0
	}
	if errors.Is(err, amocrm.ErrTransport) {
		return "network_error", 0
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "network_error", 0
	}
	return "internal_error", 0
}

// CompleteReceipt executes inside jobs.CompleteWithObserver, after the normal
// worker/attempt/lease fence succeeded. Job, receipt and admin audit commit once.
func CompleteReceipt(ctx context.Context, tx jobs.TxExecutor, job jobs.Job, raw json.RawMessage) error {
	var result executionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.State != "succeeded" && result.State != "partial" {
		return errors.New("invalid admin completion state")
	}
	data, err := json.Marshal(result.Result)
	if err != nil {
		return err
	}
	var errorJSON []byte
	if result.Error != nil {
		errorJSON, _ = json.Marshal(result.Error)
	}
	return finalizeReceipt(ctx, tx, job, result.State, result.Outcome, data, errorJSON)
}

func FailReceipt(ctx context.Context, tx jobs.TxExecutor, job jobs.Job, failure jobs.Failure, status jobs.Status) error {
	if status == jobs.StatusRetry {
		return nil
	}
	state := "unknown_outcome"
	if failure.Code == "invalid_admin_job" {
		state = "failed"
	}
	errJSON, _ := json.Marshal(&Error{Code: state, Message: "worker did not persist a confirmed command outcome; inspect the target before retrying"})
	return finalizeReceipt(ctx, tx, job, state, state, []byte(`{}`), errJSON)
}

func finalizeReceipt(ctx context.Context, tx jobs.TxExecutor, job jobs.Job, state, outcome string, result, errorJSON []byte) error {
	_, err := tx.Exec(ctx, `WITH changed AS (
		UPDATE admin_commands SET state=$2,outcome=$3,result=result||$4::jsonb,error=$5,finished_at=now()
		WHERE job_id=$1 AND state IN ('pending','running')
		RETURNING id,actor_id,target_type,target_id,command,installation_id
	) INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata)
	SELECT installation_id,'admin',actor_id,'admin.command.'||$2,target_type,target_id,
		jsonb_build_object('receipt_id',id,'state',$2::text,'command',command) FROM changed`, job.ID, state, outcome, result, nullable(errorJSON))
	return err
}
