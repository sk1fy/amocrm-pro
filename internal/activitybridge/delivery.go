package activitybridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"time"
)

// RunDelivery owns one bounded Core delivery slot, independent of Core product
// job slots and CRM Events' scheduler. Expired leases are reclaimable by another
// process; an old sender cannot finalize the new attempt.
func (b *Bridge) RunDelivery(ctx context.Context) error {
	return b.runDelivery(ctx, b.DeliverOne)
}

func (b *Bridge) runDelivery(ctx context.Context, deliver func(context.Context) (bool, error)) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		worked, err := deliver(ctx)
		if err != nil && ctx.Err() == nil {
			b.logger.ErrorContext(ctx, "Activity delivery iteration failed", "code", serviceapi.ErrorCode(err))
		}
		// Drain available work serially. Failures and an empty queue back off;
		// a receiver retry already carries its own durable run_after deadline.
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (b *Bridge) DeliverOne(ctx context.Context) (bool, error) {
	// An executor may die after claiming the final allowed attempt. Finalize
	// that expired lease as observable dead-letter without sending attempt N+1.
	if _, err := b.pool.Exec(ctx, `WITH exhausted AS (
 SELECT command_id FROM activity_command_outbox WHERE attempts>=max_attempts AND
 ((status='pending_delivery' AND run_after<=now()) OR (status='delivering' AND leased_until<now()))
 ORDER BY run_after LIMIT 100 FOR UPDATE SKIP LOCKED
) UPDATE activity_command_outbox outbox SET status='failed',error_code='delivery_attempts_exhausted',lease_token=NULL,leased_until=NULL,updated_at=now()
 FROM exhausted WHERE outbox.command_id=exhausted.command_id`); err != nil {
		return false, err
	}
	var id, lease uuid.UUID
	var scope serviceapi.Scope
	var actor int64
	var target string
	var payload []byte
	var attempts, maxAttempts int
	lease = uuid.New()
	err := b.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT command_id FROM activity_command_outbox
 WHERE (status='pending_delivery' AND run_after<=now()) OR (status='delivering' AND leased_until<now())
 ORDER BY run_after,command_id FOR UPDATE SKIP LOCKED LIMIT 1
), claimed AS (
 UPDATE activity_command_outbox outbox SET status='delivering',attempts=attempts+1,lease_token=$1,leased_until=now()+interval '30 seconds',updated_at=now()
 FROM candidate WHERE outbox.command_id=candidate.command_id
 RETURNING outbox.*
) SELECT claimed.command_id,receipt.installation_id,receipt.integration_id,receipt.actor_id,receipt.target,claimed.payload,claimed.attempts,claimed.max_attempts
 FROM claimed JOIN activity_command_receipts receipt USING(command_id)`, lease).Scan(&id, &scope.InstallationID, &scope.IntegrationID, &actor, &target, &payload, &attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	action := serviceapi.ActionSettings
	if target == serviceapi.EventsService {
		action = serviceapi.ActionSync
	}
	auth, deliveryErr := b.policy.Issue(callCtx, serviceapi.IssueRequest{Scope: scope, ActorID: actor, Consumer: serviceapi.ActivityService, RequestID: id.String(), Grants: serviceapi.UserGrantsFor(target, action)})
	var operation serviceapi.Operation
	if deliveryErr == nil {
		switch target {
		case serviceapi.EventsService:
			var command serviceapi.Command
			if err := json.Unmarshal(payload, &command); err != nil {
				deliveryErr = serviceapi.Fail(serviceapi.InvalidArgument, "invalid durable sync command")
			} else {
				command.Auth = auth
				command.CommandID = id.String()
				operation, deliveryErr = b.events.Apply(callCtx, command)
			}
		case serviceapi.ActivityService:
			var settings serviceapi.Settings
			if err := json.Unmarshal(payload, &settings); err != nil {
				deliveryErr = serviceapi.Fail(serviceapi.InvalidArgument, "invalid durable settings command")
			} else {
				operation, deliveryErr = b.activity.Configure(callCtx, serviceapi.SettingsCommand{Auth: auth, CommandID: id.String(), Settings: settings})
			}
		default:
			deliveryErr = serviceapi.Fail(serviceapi.InvalidArgument, "unknown command target")
		}
		if deliveryErr == nil && (operation.ID != id.String() || operation.CommandID != id.String()) {
			deliveryErr = serviceapi.Fail(serviceapi.Internal, "receiver returned another operation")
		}
	}
	cancel()
	status := "accepted"
	code := ""
	delay := time.Duration(0)
	if deliveryErr != nil {
		code = string(serviceapi.ErrorCode(deliveryErr))
		status = "pending_delivery"
		if attempts >= maxAttempts || permanent(serviceapi.ErrorCode(deliveryErr)) {
			status = "failed"
		}
		delay = time.Duration(1<<min(attempts, 8)) * time.Second
	}
	// Detached, short finalization survives cancellation of the delivery RPC.
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer finishCancel()
	tag, err := b.pool.Exec(finishCtx, `UPDATE activity_command_outbox SET status=$3,error_code=NULLIF($4,''),lease_token=NULL,leased_until=NULL,run_after=now()+($5*interval '1 millisecond'),updated_at=now() WHERE command_id=$1 AND lease_token=$2 AND status='delivering' AND leased_until>now()`, id, lease, status, code, delay.Milliseconds())
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() != 1 {
		return true, serviceapi.Fail(serviceapi.Conflict, "delivery lease lost")
	}
	if deliveryErr != nil {
		b.logger.WarnContext(finishCtx, "Activity command delivery deferred or rejected", "command_id", id.String(), "code", code, "delivery_state", status, "attempt", attempts)
	}
	return true, nil
}

func permanent(code serviceapi.Code) bool {
	return code == serviceapi.InvalidArgument || code == serviceapi.PermissionDenied || code == serviceapi.Conflict || code == serviceapi.NotFound
}

func (b *Bridge) Operation(ctx context.Context, p widgetauth.Principal, id string) (Receipt, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return Receipt{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	var target, state string
	var code *string
	err = b.pool.QueryRow(ctx, `SELECT receipt.target,outbox.status,outbox.error_code FROM activity_command_receipts receipt JOIN activity_command_outbox outbox USING(command_id) WHERE receipt.command_id=$1 AND receipt.installation_id=$2 AND receipt.integration_id=$3 AND receipt.actor_id=$4`, parsed, p.InstallationID, p.IntegrationID, p.UserID).Scan(&target, &state, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, serviceapi.Fail(serviceapi.NotFound, "operation not found")
	}
	if err != nil {
		return Receipt{}, err
	}
	// Scope and actor are already constrained by the receipt lookup. Issue only
	// the owning service's operation grant, even while delivery is pending.
	auth, err := b.auth(ctx, p, id, target, serviceapi.ActionOperation)
	if err != nil {
		return Receipt{}, err
	}
	if state == "delivering" {
		state = "pending_delivery"
	}
	result := Receipt{CommandID: id, OperationID: id, State: state, DeliveryState: state}
	if code != nil {
		result.ErrorCode = *code
	}
	if state != "accepted" {
		return result, nil
	}
	request := serviceapi.OperationRequest{Auth: auth, OperationID: id}
	var op serviceapi.Operation
	if target == serviceapi.EventsService {
		op, err = b.events.Operation(ctx, request)
	} else {
		op, err = b.activity.Operation(ctx, request)
	}
	if err != nil {
		// Accepted remains truthful during owner outage; do not present a false
		// final failure or completion when only status observation failed.
		result.ErrorCode = string(serviceapi.ErrorCode(err))
		return result, nil
	}
	result.State = op.State
	result.ErrorCode = op.ErrorCode
	result.Operation = &op
	return result, nil
}

// SetPilot is an operator-only control, intentionally absent from widget API.
// Disabling does not erase history or receiver dedup. Live policy blocks new
// work at the next bounded page and rejects queued Core deliveries.
func SetPilot(ctx context.Context, pool *pgxpool.Pool, installationID uuid.UUID, enabled bool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `INSERT INTO activity_pilots(installation_id,enabled) VALUES($1,$2) ON CONFLICT(installation_id) DO UPDATE SET enabled=EXCLUDED.enabled,updated_at=now()`, installationID, enabled); err != nil {
		return err
	}
	metadata, _ := json.Marshal(map[string]bool{"enabled": enabled})
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id,metadata) VALUES($1,'operator','activity-cli','activity.pilot','installation',$2,$3)`, installationID, installationID.String(), metadata); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetryDelivery keeps the same command ID and payload, including when the
// previous receiver response was lost. It never resets receiver idempotency.
func RetryDelivery(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var installation uuid.UUID
	err = tx.QueryRow(ctx, `UPDATE activity_command_outbox outbox SET status='pending_delivery',attempts=0,run_after=now(),error_code=NULL,updated_at=now() FROM activity_command_receipts receipt WHERE outbox.command_id=$1 AND receipt.command_id=outbox.command_id AND outbox.status='failed' RETURNING receipt.installation_id`, id).Scan(&installation)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.Fail(serviceapi.NotFound, "failed delivery not found")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id) VALUES($1,'operator','activity-cli','activity.delivery.retry','command',$2)`, installation, id.String()); err != nil {
		return fmt.Errorf("audit delivery retry: %w", err)
	}
	return tx.Commit(ctx)
}
