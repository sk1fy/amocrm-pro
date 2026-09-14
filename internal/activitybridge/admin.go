package activitybridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

func (b *Bridge) AdminIssue(ctx context.Context, scope serviceapi.Scope, requestID, audience, action string) (serviceapi.Auth, error) {
	if b == nil || b.policy == nil {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Unavailable, "policy is unavailable")
	}
	return b.policy.Issue(ctx, serviceapi.IssueRequest{
		Scope: scope, Kind: serviceapi.PrincipalKindOperator, ActorID: 0,
		Consumer: serviceapi.ActivityService, RequestID: requestID,
		Grants: serviceapi.UserGrantsFor(audience, action),
	})
}

func (b *Bridge) AdminSettings(ctx context.Context, scope serviceapi.Scope, requestID string) (serviceapi.Settings, error) {
	if err := b.requireActivity(); err != nil {
		return serviceapi.Settings{}, err
	}
	auth, err := b.AdminIssue(ctx, scope, requestID, serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return serviceapi.Settings{}, err
	}
	return b.activity.Settings(ctx, auth)
}

func (b *Bridge) AdminConfigure(ctx context.Context, scope serviceapi.Scope, requestID string, command serviceapi.SettingsCommand) (serviceapi.Settings, error) {
	if err := b.requireActivity(); err != nil {
		return serviceapi.Settings{}, err
	}
	auth, err := b.AdminIssue(ctx, scope, requestID, serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return serviceapi.Settings{}, err
	}
	command.Auth = auth
	if _, err := b.activity.Configure(ctx, command); err != nil {
		return serviceapi.Settings{}, err
	}
	return b.activity.Settings(ctx, auth)
}

func (b *Bridge) AdminStatus(ctx context.Context, scope serviceapi.Scope, requestID string) (serviceapi.SyncStatus, error) {
	if err := b.requireEvents(); err != nil {
		return serviceapi.SyncStatus{}, err
	}
	auth, err := b.AdminIssue(ctx, scope, requestID, serviceapi.EventsService, serviceapi.ActionStatus)
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	return b.events.Status(ctx, auth)
}

func (b *Bridge) AdminOperation(ctx context.Context, scope serviceapi.Scope, requestID, operationID string) (Receipt, error) {
	if b == nil || b.pool == nil {
		return Receipt{}, serviceapi.Fail(serviceapi.Unavailable, "activity admission is unavailable")
	}
	parsed, err := uuid.Parse(operationID)
	if err != nil {
		return Receipt{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	var target, state string
	var code *string
	err = b.pool.QueryRow(ctx, `SELECT receipt.target,outbox.status,outbox.error_code FROM activity_command_receipts receipt JOIN activity_command_outbox outbox USING(command_id) WHERE receipt.command_id=$1 AND receipt.installation_id=$2 AND receipt.integration_id=$3`, parsed, scope.InstallationID, scope.IntegrationID).Scan(&target, &state, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	if err != nil {
		return Receipt{}, err
	}
	auth, err := b.AdminIssue(ctx, scope, requestID, target, serviceapi.ActionOperation)
	if err != nil {
		return Receipt{}, err
	}
	if state == "delivering" {
		state = "pending_delivery"
	}
	if state == deliveryStatusExpired {
		state = "failed"
		if code == nil {
			expired := ErrorDeliveryExpired
			code = &expired
		}
	}
	result := Receipt{CommandID: operationID, OperationID: operationID, State: state, DeliveryState: state}
	if code != nil {
		result.ErrorCode = *code
	}
	if state != "accepted" {
		return result, nil
	}
	request := serviceapi.OperationRequest{Auth: auth, OperationID: operationID}
	var op serviceapi.Operation
	if target == serviceapi.EventsService {
		if err := b.requireEvents(); err != nil {
			return Receipt{}, err
		}
		op, err = b.events.Operation(ctx, request)
	} else {
		if err := b.requireActivity(); err != nil {
			return Receipt{}, err
		}
		op, err = b.activity.Operation(ctx, request)
	}
	if err != nil {
		result.ErrorCode = string(serviceapi.ErrorCode(err))
		return result, nil
	}
	result.State = op.State
	result.ErrorCode = op.ErrorCode
	result.Operation = &op
	return result, nil
}

func (b *Bridge) AdmitAdmin(ctx context.Context, scope serviceapi.Scope, actorAdmin, key, target, action string, payload, hashInput []byte) (Receipt, error) {
	if b == nil || b.pool == nil {
		return Receipt{}, serviceapi.Fail(serviceapi.Unavailable, "activity admission is unavailable")
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer rollback(tx)
	receipt, err := b.admitAdminTx(ctx, tx, scope, actorAdmin, key, target, action, payload, hashInput)
	if err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

func (b *Bridge) AdmitAdminTx(ctx context.Context, tx pgx.Tx, scope serviceapi.Scope, actorAdmin, key, target, action string, payload, hashInput []byte) (Receipt, error) {
	if b == nil {
		return Receipt{}, serviceapi.Fail(serviceapi.Unavailable, "activity admission is unavailable")
	}
	return b.admitAdminTx(ctx, tx, scope, actorAdmin, key, target, action, payload, hashInput)
}

func (b *Bridge) admitAdminTx(ctx context.Context, tx pgx.Tx, scope serviceapi.Scope, actorAdmin, key, target, action string, payload, hashInput []byte) (Receipt, error) {
	if !validKey(key) {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "Idempotency-Key must be 1..128 visible characters")
	}
	if actorAdmin == "" {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "admin actor is required")
	}
	requestHash := adminCommandHash(scope, target, action, hashInput)
	keyHash := sha256.Sum256([]byte(key))
	id := uuid.New()
	if err := requireActivityEnabled(ctx, tx, scope.InstallationID); err != nil {
		return Receipt{}, err
	}
	var marker int
	err := tx.QueryRow(ctx, `SELECT 1 FROM activity_pilots pilot JOIN installations installation ON installation.id=pilot.installation_id WHERE pilot.installation_id=$1 AND installation.integration_id=$2 AND pilot.enabled FOR SHARE OF pilot`, scope.InstallationID, scope.IntegrationID).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, serviceapi.Fail(serviceapi.PermissionDenied, "activity pilot is disabled")
	}
	if err != nil {
		return Receipt{}, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO activity_command_receipts(command_id,installation_id,integration_id,actor_id,target,action,key_hash,request_hash) VALUES($1,$2,$3,0,$4,$5,$6,$7) ON CONFLICT(installation_id,target,action,key_hash) DO NOTHING`, id, scope.InstallationID, scope.IntegrationID, target, action, keyHash[:], requestHash[:])
	if err != nil {
		return Receipt{}, err
	}
	replayed := tag.RowsAffected() == 0
	if replayed {
		var existingHash []byte
		err = tx.QueryRow(ctx, `SELECT command_id,request_hash FROM activity_command_receipts WHERE installation_id=$1 AND target=$2 AND action=$3 AND key_hash=$4`, scope.InstallationID, target, action, keyHash[:]).Scan(&id, &existingHash)
		if err != nil {
			return Receipt{}, err
		}
		if !bytes.Equal(existingHash, requestHash[:]) {
			return Receipt{}, serviceapi.Fail(serviceapi.Conflict, "idempotency key has different content")
		}
	} else {
		if _, err := tx.Exec(ctx, `INSERT INTO activity_command_outbox(command_id,payload) VALUES($1,$2)`, id, payload); err != nil {
			return Receipt{}, err
		}
		metadata, _ := json.Marshal(map[string]string{"target": target, "action": action})
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'admin',$2,'activity.command.accepted','command',$3,$4)`, scope.InstallationID, actorAdmin, id.String(), metadata); err != nil {
			return Receipt{}, err
		}
	}
	return Receipt{CommandID: id.String(), OperationID: id.String(), State: "pending_delivery", DeliveryState: "pending_delivery", Replayed: replayed}, nil
}

func adminCommandHash(scope serviceapi.Scope, target, action string, hashInput []byte) [32]byte {
	canonical := fmt.Sprintf("admin-v1\x00%s\x00%s\x00%s\x00%s\x00%s", scope.IntegrationID, scope.InstallationID, target, action, hashInput)
	return sha256.Sum256([]byte(canonical))
}

func (b *Bridge) AdminListPanels(ctx context.Context, scope serviceapi.Scope, requestID string) ([]serviceapi.ManagedPanel, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return nil, err
	}
	return b.activity.ListPanels(ctx, auth)
}

func (b *Bridge) AdminCreatePanel(ctx context.Context, scope serviceapi.Scope, requestID string, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	command.Auth = auth
	return b.activity.CreatePanel(ctx, command)
}

func (b *Bridge) AdminGetPanel(ctx context.Context, scope serviceapi.Scope, requestID string, panelID uuid.UUID) (serviceapi.ManagedPanel, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return b.activity.GetManagedPanel(ctx, serviceapi.PanelRef{Auth: auth, PanelID: panelID})
}

func (b *Bridge) AdminPatchPanel(ctx context.Context, scope serviceapi.Scope, requestID string, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	command.Auth = auth
	if command.CommandID == "" {
		command.CommandID = requestID
	}
	return b.activity.PatchPanel(ctx, command)
}

func (b *Bridge) AdminRotateShare(ctx context.Context, scope serviceapi.Scope, requestID string, command serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	command.Auth = auth
	command.Rotate = true
	return b.activity.RotateShareLink(ctx, command)
}

func (b *Bridge) AdminListEmployees(ctx context.Context, scope serviceapi.Scope, requestID string) (serviceapi.Directory, error) {
	auth, err := b.issuePanelAuth(ctx, scope, requestID)
	if err != nil {
		return serviceapi.Directory{}, err
	}
	return b.activity.ListEmployees(ctx, auth)
}

func (b *Bridge) issuePanelAuth(ctx context.Context, scope serviceapi.Scope, requestID string) (serviceapi.Auth, error) {
	if err := b.requireActivity(); err != nil {
		return serviceapi.Auth{}, err
	}
	return b.AdminIssue(ctx, scope, requestID, serviceapi.ActivityService, serviceapi.ActionPanels)
}

func (b *Bridge) requireActivity() error {
	if b == nil || b.activity == nil {
		return serviceapi.Fail(serviceapi.Unavailable, "activity is unavailable")
	}
	return nil
}

func (b *Bridge) requireEvents() error {
	if b == nil || b.events == nil {
		return serviceapi.Fail(serviceapi.Unavailable, "crm-events is unavailable")
	}
	return nil
}
