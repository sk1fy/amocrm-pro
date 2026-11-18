package productplatform

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type initialTermPolicy struct {
	rule, evidence string
	version        int64
}

func grantInitialTerm(ctx context.Context, tx pgx.Tx, account, product uuid.UUID, install wire.Event) error {
	var policy initialTermPolicy
	err := tx.QueryRow(ctx, `
		SELECT rule,evidence,version FROM platform_lifecycle.initial_term_policies
		WHERE product_id=$1`, product).Scan(&policy.rule, &policy.evidence, &policy.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	until, source, err := policy.term(install.OccurredAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.terms(
			account_id,product_id,valid_until,source,policy_version,policy_rule,policy_evidence,grant_event_id,granted_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`, account, product, until, source, policy.version, policy.rule, policy.evidence, install.ID, install.OccurredAt)
	return err
}

func (p initialTermPolicy) term(installedAt time.Time) (time.Time, string, error) {
	start := installedAt.UTC()
	switch p.rule {
	case "free":
		year, month, day := start.Date()
		year += 10
		day = min(day, time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day())
		return time.Date(year, month, day, start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), time.UTC), "free", nil
	case "top_90d":
		return start.AddDate(0, 0, 90), "own_trial", nil
	case "pro_7d":
		return start.AddDate(0, 0, 7), "own_trial", nil
	case "pro_14d":
		return start.AddDate(0, 0, 14), "own_trial", nil
	default:
		return time.Time{}, "", errors.New("unknown initial term rule")
	}
}
