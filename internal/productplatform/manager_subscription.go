package productplatform

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

type subscriptionInput struct {
	Action string `json:"action"`
	Plan   string `json:"plan,omitempty"`
	EndsOn string `json:"ends_on,omitempty"`
	Seats  int    `json:"seats,omitempty"`
	Deal   string `json:"deal_id,omitempty"`
}

var subscriptionDeal = regexp.MustCompile(`^[1-9][0-9]{0,14}$`)

func parseSubscription(raw []byte) (subscriptionInput, error) {
	var in subscriptionInput
	if err := readManagerObject(raw, map[string]any{"action": &in.Action, "plan": &in.Plan, "ends_on": &in.EndsOn, "seats": &in.Seats, "deal_id": &in.Deal}); err != nil {
		return in, err
	}
	switch in.Action {
	case "grant", "renew":
		if _, err := time.Parse(time.DateOnly, in.EndsOn); err != nil || (in.Plan != "top" && in.Plan != "unified") || !subscriptionDeal.MatchString(in.Deal) {
			return in, errManagerInput
		}
	case "seats", "revoke":
		if in.EndsOn != "" || in.Plan != "" || in.Deal != "" {
			return in, errManagerInput
		}
	default:
		return in, errManagerInput
	}
	if in.Action == "revoke" && in.Seats != 0 || in.Action != "revoke" && (in.Seats < 1 || in.Seats > 1000000) {
		return in, errManagerInput
	}
	return in, nil
}

func (m managerHTTP) changeSubscription(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	key, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, versionErr := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if idErr != nil || keyErr != nil || versionErr != nil || contentType != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	input, err := parseSubscription(raw)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	cmd := managerCommand{ID: env.Command, Key: key, Subject: env.Actor.Subject, RequestID: env.RequestID,
		Account: id, Kind: subscriptionCommandKind, Version: version}
	outcome, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return applySubscription(ctx, tx, cmd, input)
	})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}

func applySubscription(ctx context.Context, tx pgx.Tx, cmd managerCommand, in subscriptionInput) (managerOutcome, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO platform_manager.account_profiles(account_id) VALUES($1) ON CONFLICT DO NOTHING`, cmd.Account); err != nil {
		return managerOutcome{}, err
	}
	var version int64
	var today string
	if err := tx.QueryRow(ctx, `SELECT version,to_char(statement_timestamp() AT TIME ZONE 'Europe/Moscow','YYYY-MM-DD') FROM platform_manager.account_profiles WHERE account_id=$1 FOR UPDATE`, cmd.Account).Scan(&version, &today); err != nil {
		return managerOutcome{}, err
	}
	if version != cmd.Version {
		return managerVersionConflict(cmd, version)
	}
	var prior subscriptionInput
	err := tx.QueryRow(ctx, `SELECT action,plan,to_char(ends_on,'YYYY-MM-DD'),seats,deal_id FROM platform_manager.account_subscriptions WHERE account_id=$1`, cmd.Account).Scan(&prior.Action, &prior.Plan, &prior.EndsOn, &prior.Seats, &prior.Deal)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return managerOutcome{}, err
	}
	exists := err == nil && prior.Action != "revoke"
	var kind string
	switch in.Action {
	case "grant":
		if exists && prior.EndsOn >= today || in.EndsOn < today {
			return rejectRenewal(cmd, "validation_failed")
		}
		kind = "subscription_granted"
	case "renew":
		if !exists || in.Plan != prior.Plan || in.EndsOn <= prior.EndsOn || in.EndsOn < today {
			return rejectRenewal(cmd, "target_not_extended")
		}
		kind = "subscription_renewed"
	case "seats", "revoke":
		if !exists {
			return rejectRenewal(cmd, "validation_failed")
		}
		in.Plan, in.Deal, in.EndsOn = prior.Plan, prior.Deal, prior.EndsOn
		kind = "subscription_seats"
		if in.Action == "revoke" {
			in.Seats = prior.Seats
			kind = "subscription_revoked"
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE platform_manager.account_profiles SET version=version+1,
 classification=CASE WHEN $2 THEN 'Наш клиент' ELSE classification END,partner_name=CASE WHEN $2 THEN NULL ELSE partner_name END WHERE account_id=$1`, cmd.Account, in.Plan == "top" && in.Action == "grant"); err != nil {
		return managerOutcome{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_manager.account_subscription_revisions(account_id,version,action,plan,ends_on,seats,deal_id,command_id)
 VALUES($1,$2,$3,$4,$5::text::date,$6,$7,$8)`, cmd.Account, version+1, in.Action, in.Plan, in.EndsOn, in.Seats, in.Deal, cmd.ID); err != nil {
		return managerOutcome{}, err
	}
	if _, err = recordAccountChange(ctx, tx, cmd, accountChange{Account: cmd.Account, Kind: kind, Version: version + 1, Details: in}); err != nil {
		return managerOutcome{}, err
	}
	detail, version, err := readAccountDetail(ctx, tx, cmd.Account, []accountCapability{})
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, version, detail)
}
