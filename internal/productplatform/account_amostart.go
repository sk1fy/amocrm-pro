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

func (o *AccountObserver) readAmoStart(ctx context.Context, account uuid.UUID) (int64, amostart.License, queue.Outcome) {
	var license amostart.License
	if o.amoStart == nil {
		return 0, license, queue.Outcome{FailureCode: "amostart_not_configured"}
	}
	var amoAccount int64
	err := o.pool.QueryRow(ctx, `
		SELECT a.amo_account_id FROM platform_manager.visible_accounts a
		WHERE a.account_id=$1 AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p
		    WHERE p.account_id=a.account_id AND p.classification='Запрет работы')`, account).Scan(&amoAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, license, queue.Outcome{FailureCode: accountSourceChanged}
	}
	if err != nil {
		return 0, license, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
	license, err = o.amoStart.Lookup(ctx, amoAccount)
	switch {
	case err == nil && amoAccount > 0 && managerText(license.Tariff, 1, 512) && !license.Until.IsZero():
		return amoAccount, license, queue.Outcome{}
	case errors.Is(err, amostart.ErrRateLimited):
		return 0, license, queue.Outcome{FailureCode: "source_rate_limited", Retry: true, RetryAfter: time.Minute}
	case errors.Is(err, amostart.ErrAuthorization):
		return 0, license, queue.Outcome{FailureCode: "amostart_authorization_failed"}
	case err == nil || errors.Is(err, amostart.ErrNoLicense):
		return 0, license, queue.Outcome{FailureCode: "amostart_license_unavailable"}
	default:
		return 0, license, queue.Outcome{FailureCode: "amostart_unavailable"}
	}
}

func publishAmoStart(ctx context.Context, tx pgx.Tx, account uuid.UUID, amoAccount int64, license amostart.License) (queue.Outcome, error) {
	result, err := tx.Exec(ctx, `
	    INSERT INTO platform_manager.account_amostart_licenses(account_id,license_until,tariff,observed_at,evidence_id)
	    SELECT a.account_id,$3,$4,statement_timestamp(),$5 FROM platform_manager.visible_accounts a
	    WHERE a.account_id=$1 AND a.amo_account_id=$2 AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p
	        WHERE p.account_id=a.account_id AND p.classification='Запрет работы')
	    ON CONFLICT(account_id) DO UPDATE SET license_until=EXCLUDED.license_until,tariff=EXCLUDED.tariff,
	        observed_at=EXCLUDED.observed_at,evidence_id=EXCLUDED.evidence_id`,
		account, amoAccount, license.Until, license.Tariff, uuid.New())
	if err == nil && result.RowsAffected() == 0 {
		return queue.Outcome{FailureCode: accountSourceChanged}, nil
	}
	return queue.Outcome{}, err
}
