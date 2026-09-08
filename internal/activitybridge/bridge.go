// Package activitybridge is Core's widget admission and reliable command
// delivery adapter. Only this adapter uses Core persistence; it never queries a
// product database. All network calls happen outside Core write transactions.
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

type Bridge struct {
	pool     *pgxpool.Pool
	policy   serviceapi.Policy
	activity serviceapi.Activity
	events   serviceapi.CRMEvents
	logger   *slog.Logger
}

func New(pool *pgxpool.Pool, policy serviceapi.Policy, product serviceapi.Activity, events serviceapi.CRMEvents) *Bridge {
	return &Bridge{pool: pool, policy: policy, activity: product, events: events, logger: slog.Default()}
}

type Receipt struct {
	CommandID     string                `json:"command_id"`
	OperationID   string                `json:"operation_id"`
	State         string                `json:"state"`
	DeliveryState string                `json:"delivery_state"`
	ErrorCode     string                `json:"error_code,omitempty"`
	Operation     *serviceapi.Operation `json:"operation,omitempty"`
	Replayed      bool                  `json:"-"`
}

type SyncInput struct {
	Kind string `json:"kind"`
	From int64  `json:"from,omitempty"`
	To   int64  `json:"to,omitempty"`
}

func (b *Bridge) auth(ctx context.Context, p widgetauth.Principal, requestID, audience, action string) (serviceapi.Auth, error) {
	if p.IntegrationID == uuid.Nil || p.InstallationID == uuid.Nil || p.UserID <= 0 {
		return serviceapi.Auth{}, serviceapi.Fail(serviceapi.Unauthenticated, "verified actor required")
	}
	return b.policy.Issue(ctx, serviceapi.IssueRequest{Scope: serviceapi.Scope{IntegrationID: p.IntegrationID, InstallationID: p.InstallationID}, ActorID: p.UserID, Consumer: serviceapi.ActivityService, RequestID: requestID, Grants: serviceapi.UserGrantsFor(audience, action)})
}

func (b *Bridge) Panel(ctx context.Context, p widgetauth.Principal, q serviceapi.Query) (serviceapi.Panel, error) {
	auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionPanel)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	q.Auth = auth
	result, err := b.activity.Panel(ctx, q)
	if err != nil {
		return serviceapi.Panel{}, err
	}
	if err := serviceapi.RequireQueryVersion(q, result.Data); err != nil {
		return serviceapi.Panel{}, err
	}
	return result, nil
}
func (b *Bridge) GetEvent(ctx context.Context, p widgetauth.Principal, eventID string) (serviceapi.Event, error) {
	request := serviceapi.EventRequest{EventID: eventID}
	if err := serviceapi.ValidateEventRequest(request); err != nil {
		return serviceapi.Event{}, err
	}
	if presenter, ok := b.activity.(serviceapi.EventPresenter); ok {
		auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionPanel)
		if err != nil {
			return serviceapi.Event{}, err
		}
		request.Auth = auth
		event, err := presenter.EventCard(ctx, request)
		if err == nil || serviceapi.ErrorCode(err) != serviceapi.Unavailable {
			return event, err
		}
	}
	auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.EventsService, serviceapi.ActionRead)
	if err != nil {
		return serviceapi.Event{}, err
	}
	reader, ok := b.events.(serviceapi.EventReader)
	if !ok {
		return serviceapi.Event{}, serviceapi.Fail(serviceapi.Unavailable, "event detail reader unavailable")
	}
	request.Auth = auth
	return reader.GetEvent(ctx, request)
}
func (b *Bridge) Settings(ctx context.Context, p widgetauth.Principal) (serviceapi.Settings, error) {
	auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return serviceapi.Settings{}, err
	}
	return b.activity.Settings(ctx, auth)
}
func (b *Bridge) Status(ctx context.Context, p widgetauth.Principal) (serviceapi.SyncStatus, error) {
	auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.EventsService, serviceapi.ActionStatus)
	if err != nil {
		return serviceapi.SyncStatus{}, err
	}
	return b.events.Status(ctx, auth)
}

func (b *Bridge) Configure(ctx context.Context, p widgetauth.Principal, key string, settings serviceapi.Settings) (Receipt, error) {
	if err := serviceapi.ValidateSettings(settings); err != nil {
		return Receipt{}, err
	}
	if _, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionSettings); err != nil {
		return Receipt{}, err
	}
	payload, _ := json.Marshal(settings)
	return b.admit(ctx, p, key, serviceapi.ActivityService, serviceapi.ActionSettings, payload)
}

func (b *Bridge) Sync(ctx context.Context, p widgetauth.Principal, key string, input SyncInput) (Receipt, error) {
	if input.Kind == "" {
		input.Kind = "sync"
	}
	if input.Kind != "sync" && input.Kind != "enable" && input.Kind != "disable" && input.Kind != "backfill" {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "unsupported sync kind")
	}
	if input.Kind == "backfill" {
		if input.From <= 0 || input.To <= input.From || input.To > time.Now().Unix() || input.To-input.From > 31*86400 {
			return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "backfill must be a past interval of at most 31 days")
		}
	} else if input.From != 0 || input.To != 0 {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "from/to are only valid for backfill")
	}
	auth, err := b.auth(ctx, p, uuid.NewString(), serviceapi.ActivityService, serviceapi.ActionSettings)
	if err != nil {
		return Receipt{}, err
	}
	inputJSON, _ := json.Marshal(input)
	// A retry uses the original settings snapshot even if product defaults
	// have changed or Activity is temporarily unreachable since admission.
	keyHash := sha256.Sum256([]byte(key))
	var originalPayload, originalHash []byte
	err = b.pool.QueryRow(ctx, `SELECT outbox.payload,receipt.request_hash FROM activity_command_receipts receipt JOIN activity_command_outbox outbox USING(command_id) WHERE receipt.installation_id=$1 AND receipt.target='crm-events' AND receipt.action='sync' AND receipt.key_hash=$2`, p.InstallationID, keyHash[:]).Scan(&originalPayload, &originalHash)
	if err == nil {
		want := commandHash(p, serviceapi.EventsService, serviceapi.ActionSync, inputJSON)
		if !bytes.Equal(want[:], originalHash) {
			return Receipt{}, serviceapi.Fail(serviceapi.Conflict, "idempotency key has different content")
		}
		return b.admitHash(ctx, p, key, serviceapi.EventsService, serviceapi.ActionSync, originalPayload, inputJSON)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, err
	}
	settings, err := b.activity.Settings(ctx, auth)
	if err != nil {
		return Receipt{}, err
	}
	if input.Kind == "backfill" && input.From < time.Now().Unix()-int64(settings.RetentionDays)*86400 {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "backfill starts before retained history")
	}
	payload, _ := json.Marshal(serviceapi.Command{Kind: input.Kind, From: input.From, To: input.To, InitialDays: settings.InitialDays, RetentionDays: settings.RetentionDays})
	return b.admitHash(ctx, p, key, serviceapi.EventsService, serviceapi.ActionSync, payload, inputJSON)
}

func (b *Bridge) admit(ctx context.Context, p widgetauth.Principal, key, target, action string, payload []byte) (Receipt, error) {
	return b.admitHash(ctx, p, key, target, action, payload, payload)
}

func (b *Bridge) admitHash(ctx context.Context, p widgetauth.Principal, key, target, action string, payload, hashInput []byte) (Receipt, error) {
	if !validKey(key) {
		return Receipt{}, serviceapi.Fail(serviceapi.InvalidArgument, "Idempotency-Key must be 1..128 visible characters")
	}
	if p.TokenID == "" || p.Issuer == "" || !p.TokenRetainUntil.After(time.Now()) {
		return Receipt{}, serviceapi.Fail(serviceapi.Unauthenticated, "expired widget principal")
	}
	requestHash := commandHash(p, target, action, hashInput)
	keyHash := sha256.Sum256([]byte(key))
	id := uuid.New()
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer rollback(tx)
	if err := requireActivityEnabled(ctx, tx, p.InstallationID); err != nil {
		return Receipt{}, err
	}
	var marker int
	err = tx.QueryRow(ctx, `SELECT 1 FROM activity_pilots pilot JOIN installations installation ON installation.id=pilot.installation_id WHERE pilot.installation_id=$1 AND installation.integration_id=$2 AND pilot.enabled FOR SHARE OF pilot`, p.InstallationID, p.IntegrationID).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, serviceapi.Fail(serviceapi.PermissionDenied, "activity pilot is disabled")
	}
	if err != nil {
		return Receipt{}, err
	}
	token := p.UsedToken()
	tag, err := tx.Exec(ctx, `INSERT INTO used_widget_tokens(integration_id,jti,issuer,account_id,user_id,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(integration_id,jti) DO NOTHING`, token.IntegrationID, token.TokenID, token.Issuer, token.AccountID, token.UserID, token.ExpiresAt)
	if err != nil {
		return Receipt{}, err
	}
	if tag.RowsAffected() != 1 {
		return Receipt{}, widgetauth.ErrReplay
	}
	tag, err = tx.Exec(ctx, `INSERT INTO activity_command_receipts(command_id,installation_id,integration_id,actor_id,target,action,key_hash,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(installation_id,target,action,key_hash) DO NOTHING`, id, p.InstallationID, p.IntegrationID, p.UserID, target, action, keyHash[:], requestHash[:])
	if err != nil {
		return Receipt{}, err
	}
	replayed := tag.RowsAffected() == 0
	if replayed {
		var existingHash []byte
		err = tx.QueryRow(ctx, `SELECT command_id,request_hash FROM activity_command_receipts WHERE installation_id=$1 AND target=$2 AND action=$3 AND key_hash=$4`, p.InstallationID, target, action, keyHash[:]).Scan(&id, &existingHash)
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
	}
	if err := tx.Commit(ctx); err != nil {
		return Receipt{}, err
	}
	// This is Core acceptance only, including retries. The status endpoint is
	// authoritative for delivery and the receiver's operation lifecycle.
	return Receipt{CommandID: id.String(), OperationID: id.String(), State: "pending_delivery", DeliveryState: "pending_delivery", Replayed: replayed}, nil
}

func commandHash(p widgetauth.Principal, target, action string, payload []byte) [32]byte {
	canonical := fmt.Sprintf("v1\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s", p.IntegrationID, p.InstallationID, p.UserID, target, action, payload)
	return sha256.Sum256([]byte(canonical))
}
func validKey(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
