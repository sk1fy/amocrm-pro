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

// RedeliveryHorizon is the maximum calendar age of a Core command, measured
// from activity_command_receipts.created_at. Attempt count, backoff, operator
// retry, and executor downtime cannot extend delivery past this floor. It is
// the same 7-day HistoryHorizon as CRM Events technical-history GC (ADR-0016).
const RedeliveryHorizon = 7 * 24 * time.Hour

const (
	deliveryStatusExpired = "expired"
	ErrorDeliveryExpired  = "delivery_expired"
)

// ErrDeliveryExpired is returned when an operator retries a command older than
// RedeliveryHorizon. Attempts are left unchanged.
func ErrDeliveryExpired() error {
	return serviceapi.Fail(serviceapi.Conflict, "delivery is older than the redelivery horizon")
}

func IsDeliveryExpired(err error) bool {
	var e *serviceapi.Error
	return errors.As(err, &e) && e.Code == serviceapi.Conflict && e.Message == "delivery is older than the redelivery horizon"
}

func pastRedeliveryHorizon(createdAt, now time.Time) bool {
	return createdAt.Before(now.Add(-RedeliveryHorizon))
}

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
	// Calendar age is independent of attempts: a command accepted more than
	// RedeliveryHorizon ago is never sent, including after executor downtime.
	if _, err := b.pool.Exec(ctx, `WITH expired AS (
 SELECT outbox.command_id FROM activity_command_outbox outbox
 JOIN activity_command_receipts receipt USING(command_id)
 WHERE receipt.created_at < now()-($1*interval '1 millisecond')
   AND (outbox.status='pending_delivery' OR (outbox.status='delivering' AND outbox.leased_until<now()))
 ORDER BY receipt.created_at,outbox.command_id FOR UPDATE OF outbox SKIP LOCKED LIMIT 100
) UPDATE activity_command_outbox outbox SET status='expired',error_code='delivery_expired',lease_token=NULL,leased_until=NULL,updated_at=now()
 FROM expired WHERE outbox.command_id=expired.command_id`, RedeliveryHorizon.Milliseconds()); err != nil {
		return false, err
	}
	var id, lease uuid.UUID
	var scope serviceapi.Scope
	var actor int64
	var target string
	var payload []byte
	var attempts, maxAttempts int
	var created time.Time
	lease = uuid.New()
	err := b.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT outbox.command_id FROM activity_command_outbox outbox
 JOIN activity_command_receipts receipt USING(command_id)
 WHERE receipt.created_at >= now()-($2*interval '1 millisecond')
   AND ((outbox.status='pending_delivery' AND outbox.run_after<=now()) OR (outbox.status='delivering' AND outbox.leased_until<now()))
 ORDER BY outbox.run_after,outbox.command_id FOR UPDATE OF outbox SKIP LOCKED LIMIT 1
), claimed AS (
 UPDATE activity_command_outbox outbox SET status='delivering',attempts=attempts+1,lease_token=$1,leased_until=now()+interval '30 seconds',updated_at=now()
 FROM candidate WHERE outbox.command_id=candidate.command_id
 RETURNING outbox.*
) SELECT claimed.command_id,receipt.installation_id,receipt.integration_id,receipt.actor_id,receipt.target,claimed.payload,claimed.attempts,claimed.max_attempts,receipt.created_at
 FROM claimed JOIN activity_command_receipts receipt USING(command_id)`, lease, RedeliveryHorizon.Milliseconds()).Scan(&id, &scope.InstallationID, &scope.IntegrationID, &actor, &target, &payload, &attempts, &maxAttempts, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pastRedeliveryHorizon(created, time.Now().UTC()) {
		finishCtx, finishCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer finishCancel()
		tag, err := b.pool.Exec(finishCtx, `UPDATE activity_command_outbox SET status='expired',error_code='delivery_expired',lease_token=NULL,leased_until=NULL,updated_at=now() WHERE command_id=$1 AND lease_token=$2 AND status='delivering' AND leased_until>now()`, id, lease)
		if err != nil {
			return true, err
		}
		if tag.RowsAffected() != 1 {
			return true, serviceapi.Fail(serviceapi.Conflict, "delivery lease lost")
		}
		return true, nil
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
	if state == deliveryStatusExpired {
		state = "failed"
		if code == nil {
			expired := ErrorDeliveryExpired
			code = &expired
		}
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
// Commands older than RedeliveryHorizon are rejected and attempts are not reset.
func RetryDelivery(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var installation uuid.UUID
	var created time.Time
	var status string
	err = tx.QueryRow(ctx, `SELECT receipt.installation_id,receipt.created_at,outbox.status FROM activity_command_outbox outbox JOIN activity_command_receipts receipt USING(command_id) WHERE outbox.command_id=$1 FOR UPDATE OF outbox,receipt`, id).Scan(&installation, &created, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.Fail(serviceapi.NotFound, "failed delivery not found")
	}
	if err != nil {
		return err
	}
	if pastRedeliveryHorizon(created, time.Now().UTC()) {
		return ErrDeliveryExpired()
	}
	if status != "failed" {
		return serviceapi.Fail(serviceapi.NotFound, "failed delivery not found")
	}
	tag, err := tx.Exec(ctx, `UPDATE activity_command_outbox SET status='pending_delivery',attempts=0,run_after=now(),error_code=NULL,updated_at=now() WHERE command_id=$1 AND status='failed'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return serviceapi.Fail(serviceapi.NotFound, "failed delivery not found")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id) VALUES($1,'operator','activity-cli','activity.delivery.retry','command',$2)`, installation, id.String()); err != nil {
		return fmt.Errorf("audit delivery retry: %w", err)
	}
	return tx.Commit(ctx)
}

// Delivery is the operator-visible Core outbox row. Widget Operation() is a
// different, actor-scoped contract and is not a second retry API.
type Delivery struct {
	CommandID      uuid.UUID `json:"command_id"`
	InstallationID uuid.UUID `json:"installation_id"`
	Target         string    `json:"target"`
	Status         string    `json:"status"`
	ErrorCode      string    `json:"error_code,omitempty"`
	Attempts       int       `json:"attempts"`
	MaxAttempts    int       `json:"max_attempts"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func ListDeliveries(ctx context.Context, pool *pgxpool.Pool) ([]Delivery, error) {
	rows, err := pool.Query(ctx, `SELECT receipt.command_id,receipt.installation_id,receipt.target,outbox.status,coalesce(outbox.error_code,''),outbox.attempts,outbox.max_attempts,receipt.created_at,outbox.updated_at
 FROM activity_command_outbox outbox JOIN activity_command_receipts receipt USING(command_id)
 WHERE outbox.status IN ('failed','expired')
 ORDER BY receipt.created_at,receipt.command_id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]Delivery, 0)
	for rows.Next() {
		var item Delivery
		if err := rows.Scan(&item.CommandID, &item.InstallationID, &item.Target, &item.Status, &item.ErrorCode, &item.Attempts, &item.MaxAttempts, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

func InspectDelivery(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (Delivery, error) {
	var item Delivery
	err := pool.QueryRow(ctx, `SELECT receipt.command_id,receipt.installation_id,receipt.target,outbox.status,coalesce(outbox.error_code,''),outbox.attempts,outbox.max_attempts,receipt.created_at,outbox.updated_at
 FROM activity_command_outbox outbox JOIN activity_command_receipts receipt USING(command_id)
 WHERE outbox.command_id=$1`, id).Scan(&item.CommandID, &item.InstallationID, &item.Target, &item.Status, &item.ErrorCode, &item.Attempts, &item.MaxAttempts, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, serviceapi.Fail(serviceapi.NotFound, "delivery not found")
	}
	return item, err
}
