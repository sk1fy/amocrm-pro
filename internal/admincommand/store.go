package admincommand

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/activitybridge"
	"github.com/sk1fy/amocrm-pro/internal/integrations"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/services/leadstatus"
	"github.com/sk1fy/amocrm-pro/internal/webhook"
)

type Store struct {
	pool          *pgxpool.Pool
	integrations  *integrations.Store
	jobs          *jobs.Store
	timeout       time.Duration
	publicBaseURL string
	bridge        *activitybridge.Bridge
	rules         *leadstatus.RuleStore
}

func NewStore(pool *pgxpool.Pool, cipher integrations.Cipher, timeout time.Duration, publicBaseURL string, bridge *activitybridge.Bridge, rules *leadstatus.RuleStore) *Store {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Store{pool: pool, integrations: integrations.NewStore(pool, cipher), jobs: jobs.NewStore(pool), timeout: timeout, publicBaseURL: publicBaseURL, bridge: bridge, rules: rules}
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
		if immediateConflict(req.Command, operationErr) {
			return Receipt{}, classify(operationErr)
		}
		state, outcome = "failed", "rejected"
		safeError = classify(operationErr)
		result = map[string]any{}
		jobID, installationID = nil, nil
	} else {
		if err := domainTx.Commit(ctx); err != nil {
			return Receipt{}, err
		}
		if req.Command == "check" || req.Command == "uninstall" || req.Command == "activity-sync" {
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
	var integrationID uuid.UUID
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
		if err := tx.QueryRow(ctx, `SELECT ig.id,ig.code,i.status,ig.status FROM installations i JOIN integrations ig ON ig.id=i.integration_id WHERE i.id=$1`, id).Scan(&integrationID, &code, &status, &integrationStatus); err != nil {
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
		case "activity-configure", "activity-sync", "activity-panel-create", "activity-panel-patch", "activity-panel-rotate":
			return s.applyActivity(ctx, tx, actor, receiptID, req, p, id, integrationID, installationID)
		case "lead-status-configure":
			return s.applyLeadStatus(ctx, tx, actor, p, id, installationID)
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
	if err != nil {
		return Receipt{}, err
	}
	if err := s.refreshActivitySync(ctx, &r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

func (s *Store) applyActivity(ctx context.Context, tx pgx.Tx, actor string, receiptID uuid.UUID, req Request, p commandPayload, installation, integration uuid.UUID, installationID *uuid.UUID) (map[string]any, *uuid.UUID, *uuid.UUID, error) {
	if s.bridge == nil {
		return nil, nil, installationID, serviceapi.Fail(serviceapi.Unavailable, "activity is unavailable")
	}
	scope := serviceapi.Scope{InstallationID: installation, IntegrationID: integration}
	requestID := receiptID.String()
	switch req.Command {
	case "activity-configure":
		settings := serviceapi.Settings{InitialDays: *p.InitialDays, RetentionDays: *p.RetentionDays}
		result, err := s.bridge.AdminConfigure(ctx, scope, requestID, serviceapi.SettingsCommand{
			CommandID: requestID, Settings: settings, ExpectedUpdatedAt: p.ExpectedUpdatedAt,
		})
		if err != nil {
			if serviceapi.ErrorCode(err) == serviceapi.Conflict {
				current, readErr := s.bridge.AdminSettings(ctx, scope, requestID)
				if readErr == nil {
					return nil, nil, installationID, conflictDetails("settings were updated", map[string]any{
						"initial_days": current.InitialDays, "retention_days": current.RetentionDays, "updated_at": current.UpdatedAt,
					})
				}
			}
			return nil, nil, installationID, err
		}
		return map[string]any{"initial_days": result.InitialDays, "retention_days": result.RetentionDays, "updated_at": result.UpdatedAt}, nil, installationID, nil
	case "activity-sync":
		input := activitybridge.SyncInput{Kind: p.Kind, From: p.From, To: p.To}
		if input.Kind == "backfill" && input.To > time.Now().Unix() {
			return nil, nil, installationID, serviceapi.Fail(serviceapi.InvalidArgument, "backfill must be a past interval of at most 31 days")
		}
		settings, err := s.bridge.AdminSettings(ctx, scope, requestID)
		if err != nil {
			return nil, nil, installationID, err
		}
		if input.Kind == "backfill" && input.From < time.Now().Unix()-int64(settings.RetentionDays)*86400 {
			return nil, nil, installationID, serviceapi.Fail(serviceapi.InvalidArgument, "backfill starts before retained history")
		}
		payload, _ := json.Marshal(serviceapi.Command{Kind: input.Kind, From: input.From, To: input.To, InitialDays: settings.InitialDays, RetentionDays: settings.RetentionDays})
		inputJSON, _ := json.Marshal(input)
		receipt, err := s.bridge.AdmitAdminTx(ctx, tx, scope, actor, requestID, serviceapi.EventsService, serviceapi.ActionSync, payload, inputJSON)
		if err != nil {
			return nil, nil, installationID, err
		}
		return map[string]any{"command_id": receipt.CommandID, "operation_id": receipt.OperationID, "delivery_state": receipt.DeliveryState}, nil, installationID, nil
	case "activity-panel-create":
		panel, err := s.bridge.AdminCreatePanel(ctx, scope, requestID, serviceapi.PanelCommand{
			CommandID: requestID, Name: p.Name, EmployeeIDs: p.EmployeeIDs, DisplayWindow: *p.DisplayWindow,
			Enabled: p.Enabled, HasName: true, HasEmployees: true, HasWindow: true,
		})
		if err != nil {
			return nil, nil, installationID, err
		}
		return publicPanelResult(panel, false), nil, installationID, nil
	case "activity-panel-patch":
		panelID, _ := uuid.Parse(p.PanelID)
		command := serviceapi.PanelCommand{PanelID: panelID, Revision: *p.Revision, Enabled: p.Enabled}
		if p.Name != "" {
			command.Name = p.Name
			command.HasName = true
		}
		if p.EmployeeIDs != nil {
			command.EmployeeIDs = p.EmployeeIDs
			command.HasEmployees = true
		}
		if p.DisplayWindow != nil {
			command.DisplayWindow = *p.DisplayWindow
			command.HasWindow = true
		}
		panel, err := s.bridge.AdminPatchPanel(ctx, scope, requestID, command)
		if err != nil {
			return nil, nil, installationID, err
		}
		return publicPanelResult(panel, false), nil, installationID, nil
	case "activity-panel-rotate":
		// Rotating from admin invalidates old links; the new secret stays in
		// Activity — operators use activity-control CLI if they need the URL.
		panelID, _ := uuid.Parse(p.PanelID)
		panel, err := s.bridge.AdminRotateShare(ctx, scope, requestID, serviceapi.PanelCommand{CommandID: requestID, PanelID: panelID, Rotate: true})
		if err != nil {
			return nil, nil, installationID, err
		}
		return publicPanelResult(panel, true), nil, installationID, nil
	default:
		return nil, nil, installationID, invalid("unsupported command")
	}
}

func (s *Store) applyLeadStatus(ctx context.Context, tx pgx.Tx, actor string, p commandPayload, installation uuid.UUID, installationID *uuid.UUID) (map[string]any, *uuid.UUID, *uuid.UUID, error) {
	if s.rules == nil {
		return nil, nil, installationID, serviceapi.Fail(serviceapi.Unavailable, "lead-status is unavailable")
	}
	result, err := s.rules.ConfigureAdminTx(ctx, tx, installation, actor, leadstatus.LeadStatusRuleCommand{
		SourcePipelineID: p.SourcePipelineID, SourceStatusID: p.SourceStatusID,
		TargetPipelineID: p.TargetPipelineID, TargetStatusID: p.TargetStatusID,
		Enabled: *p.Enabled, ExpectedRevision: *p.ExpectedRevision,
	})
	if err != nil {
		return nil, nil, installationID, err
	}
	return map[string]any{
		"rule_id": result.RuleID, "source_pipeline_id": result.SourcePipelineID, "source_status_id": result.SourceStatusID,
		"target_pipeline_id": result.TargetPipelineID, "target_status_id": result.TargetStatusID,
		"enabled": result.Enabled, "revision": result.Revision,
	}, nil, installationID, nil
}

func (s *Store) refreshActivitySync(ctx context.Context, r *Receipt) error {
	if s.bridge == nil || r.Command != "activity-sync" || (r.State != "pending" && r.State != "running") {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return nil
	}
	commandID, _ := result["command_id"].(string)
	if commandID == "" {
		return nil
	}
	installationID, err := uuid.Parse(r.TargetID)
	if err != nil {
		return nil
	}
	var integrationID uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT integration_id FROM installations WHERE id=$1`, installationID).Scan(&integrationID); err != nil {
		return err
	}
	receipt, err := s.bridge.AdminOperation(ctx, serviceapi.Scope{InstallationID: installationID, IntegrationID: integrationID}, r.ID.String(), commandID)
	if err != nil {
		return nil
	}
	adminState, outcome := mapActivitySyncState(receipt)
	result["delivery_state"] = receipt.DeliveryState
	result["operation_id"] = receipt.OperationID
	if receipt.ErrorCode != "" {
		result["error_code"] = receipt.ErrorCode
	}
	encoded, _ := json.Marshal(result)
	if _, err := s.pool.Exec(ctx, `UPDATE admin_commands SET state=$2,outcome=$3,result=$4,error=$5,
		finished_at=CASE WHEN $2 IN ('succeeded','failed') THEN now() ELSE finished_at END WHERE id=$1 AND state IN ('pending','running')`,
		r.ID, adminState, outcome, encoded, activitySyncError(adminState, receipt.ErrorCode)); err != nil {
		return err
	}
	updated, _, err := loadReceipt(ctx, s.pool, `c.id=$1`, r.ID)
	if err != nil {
		return err
	}
	*r = updated
	return nil
}

func mapActivitySyncState(receipt activitybridge.Receipt) (string, string) {
	switch receipt.State {
	case "pending_delivery":
		return "pending", ""
	case "succeeded":
		return "succeeded", "completed"
	case "failed", "expired":
		return "failed", "rejected"
	default:
		return "running", ""
	}
}

func activitySyncError(state, code string) any {
	if state != "failed" || code == "" {
		return nil
	}
	encoded, _ := json.Marshal(Error{Code: code, Message: "activity sync failed"})
	return encoded
}

func publicPanelResult(panel serviceapi.ManagedPanel, rotate bool) map[string]any {
	ids := panel.EmployeeIDs
	if ids == nil {
		ids = []int64{}
	}
	out := map[string]any{
		"id": panel.ID, "name": panel.Name, "employee_ids": ids,
		"display_window": panel.DisplayWindow, "enabled": panel.Enabled,
		"revision": panel.Revision, "updated_at": panel.UpdatedAt,
		"share_url_issued": panel.ShareUrlIssued,
	}
	if rotate {
		out["view_key_version"] = panel.ViewKeyVersion
		out["share_url_issued"] = true
	}
	return out
}

func immediateConflict(command string, err error) bool {
	switch command {
	case "activity-configure", "activity-panel-patch", "lead-status-configure":
		// Reading the current settings enriches service conflicts into our
		// own Error type. Preserve the immediate 409 semantics after wrapping.
		var api *Error
		return (errors.As(err, &api) && api.Code == "conflict") ||
			serviceapi.ErrorCode(err) == serviceapi.Conflict || errors.Is(err, leadstatus.ErrRuleRevisionConflict)
	}
	return false
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
	if errors.Is(err, leadstatus.ErrRuleRevisionConflict) {
		return conflict("lead status rule revision conflict")
	}
	if errors.Is(err, leadstatus.ErrInvalidLeadStatusRule) {
		return invalid("invalid lead status rule")
	}
	switch serviceapi.ErrorCode(err) {
	case serviceapi.NotFound:
		return notFound()
	case serviceapi.Conflict:
		message := "command precondition failed"
		var se *serviceapi.Error
		if errors.As(err, &se) && se.Message != "" {
			message = se.Message
		}
		return conflict(message)
	case serviceapi.InvalidArgument:
		message := "invalid argument"
		var se *serviceapi.Error
		if errors.As(err, &se) && se.Message != "" {
			message = se.Message
		}
		return invalid(message)
	case serviceapi.PermissionDenied, serviceapi.ReauthRequired:
		return &Error{Code: "permission_denied", Message: "not permitted", Status: http.StatusForbidden}
	case serviceapi.Unavailable, serviceapi.DeadlineExceeded:
		return &Error{Code: "backend_unavailable", Message: "backend unavailable", Status: http.StatusServiceUnavailable}
	}
	return &Error{Code: "internal", Message: "command failed", Status: 500}
}
func nullable(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
