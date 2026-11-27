package productplatform

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sk1fy/amocrm-pro/internal/productplatform/amostart"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

type amoStartObservation struct {
	AmoAccount int64
	At         time.Time
	License    amostart.License
}

func (o *AccountObserver) readAmoStart(ctx context.Context, account uuid.UUID) (amoStartObservation, queue.Outcome) {
	var observation amoStartObservation
	if o.amoStart == nil {
		return observation, queue.Outcome{FailureCode: "amostart_not_configured"}
	}
	err := o.pool.QueryRow(ctx, `
		SELECT a.amo_account_id,statement_timestamp() FROM platform_manager.visible_accounts a
		WHERE a.account_id=$1 AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p
		    WHERE p.account_id=a.account_id AND p.classification='Запрет работы')`, account).Scan(&observation.AmoAccount, &observation.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return observation, queue.Outcome{FailureCode: accountSourceChanged}
	}
	if err != nil {
		return observation, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
	license, err := o.amoStart.Lookup(ctx, observation.AmoAccount)
	switch {
	case err == nil && observation.AmoAccount > 0 && managerText(license.Tariff, 1, 512) && !license.Until.IsZero():
		observation.License = license
		return observation, queue.Outcome{}
	case errors.Is(err, amostart.ErrRateLimited):
		return observation, queue.Outcome{FailureCode: "source_rate_limited", Retry: true, RetryAfter: time.Minute}
	case errors.Is(err, amostart.ErrAuthorization):
		return observation, queue.Outcome{FailureCode: "amostart_authorization_failed"}
	case err == nil || errors.Is(err, amostart.ErrNoLicense):
		return observation, queue.Outcome{FailureCode: "amostart_license_unavailable"}
	default:
		return observation, queue.Outcome{FailureCode: "amostart_unavailable"}
	}
}

func publishAmoStart(ctx context.Context, tx pgx.Tx, account uuid.UUID, observation amoStartObservation) (queue.Outcome, error) {
	result, err := tx.Exec(ctx, `
	    INSERT INTO platform_manager.account_amostart_licenses AS current(account_id,license_until,tariff,observed_at,evidence_id)
	    SELECT a.account_id,$3,$4,$5,$6 FROM platform_manager.visible_accounts a
	    WHERE a.account_id=$1 AND a.amo_account_id=$2 AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p
	        WHERE p.account_id=a.account_id AND p.classification='Запрет работы')
	    ON CONFLICT(account_id) DO UPDATE SET license_until=EXCLUDED.license_until,tariff=EXCLUDED.tariff,
	        observed_at=EXCLUDED.observed_at,evidence_id=EXCLUDED.evidence_id
	    WHERE current.observed_at<EXCLUDED.observed_at`,
		account, observation.AmoAccount, observation.License.Until, observation.License.Tariff, observation.At, uuid.New())
	if err == nil && result.RowsAffected() == 0 {
		return queue.Outcome{FailureCode: accountSourceChanged}, nil
	}
	return queue.Outcome{}, err
}
