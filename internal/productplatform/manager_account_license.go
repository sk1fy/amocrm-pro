package productplatform

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const accountLicenseEvidenceJoins = `
    LEFT JOIN platform_manager.account_tariff_fields pay ON pay.account_id=a.account_id AND pay.field='pay_type'
    LEFT JOIN platform_manager.account_tariff_fields trial ON trial.account_id=a.account_id AND trial.field='trial_end'
    LEFT JOIN LATERAL (
        SELECT trial.numeric_value,trial.observed_at,trial.value_changed_at
        WHERE lower(pay.text_value)='trial' AND (pay.trial_reset_at IS NULL OR trial.observed_at>pay.trial_reset_at)
    ) applicable ON true
    LEFT JOIN platform_manager.account_refresh_slots ids ON ids.account_id=a.account_id AND ids.component='license'
    CROSS JOIN LATERAL (
        SELECT to_timestamp(applicable.numeric_value::double precision) AS trial_end,
            CASE WHEN lower(pay.text_value)='trial' THEN GREATEST(pay.value_changed_at,applicable.value_changed_at) END AS trial_at,
            GREATEST(pay.observed_at,applicable.observed_at) AS trial_observed_at,
            CASE WHEN lower(pay.text_value)='blocked' THEN pay.value_changed_at END AS widget_blocked_at,
            CASE WHEN i.observed_at IS NULL OR ids.payment_required_at>=i.observed_at
                 THEN ids.payment_required_at END AS identity_blocked_at
    ) signals
    CROSS JOIN LATERAL (
        SELECT GREATEST(signals.widget_blocked_at,signals.identity_blocked_at) AS blocked_at,
            GREATEST(signals.trial_at,pay.trial_or_blocked_at,ids.payment_required_at,
                CASE WHEN pay.trial_or_blocked_at IS NOT NULL THEN trial.value_changed_at END) AS paid_floor_at
    ) payment`

const accountLicenseStatusJoin = `
    CROSS JOIN LATERAL (
        SELECT CASE
            WHEN ls.result_code IS NULL AND l.observed_at<=license_clock.as_of
                 AND l.observed_at+interval '24 hours'>license_clock.as_of
                 AND (l.license_until AT TIME ZONE 'Europe/Moscow')::date>=(license_clock.as_of AT TIME ZONE 'Europe/Moscow')::date
                 AND (signals.trial_end IS NULL OR l.license_until>signals.trial_end)
                 AND (payment.paid_floor_at IS NULL OR l.observed_at>payment.paid_floor_at) THEN 'paid'
            WHEN payment.blocked_at IS NOT NULL
                 AND (signals.trial_at IS NULL OR payment.blocked_at>=signals.trial_at) THEN 'unpaid'
            WHEN signals.trial_end<=license_clock.as_of THEN 'expired_pending'
            WHEN signals.trial_at IS NOT NULL THEN 'trial'
            ELSE 'unknown'
        END AS state
    ) amo_license`

const amoLicenseStatusJSON = `jsonb_strip_nulls(jsonb_build_object(
    'state',amo_license.state,
    'source',CASE amo_license.state
        WHEN 'paid' THEN 'amostart'
        WHEN 'unpaid' THEN CASE WHEN payment.blocked_at=signals.widget_blocked_at THEN 'widget' ELSE 'amocrm' END
        WHEN 'trial' THEN 'widget'
        WHEN 'expired_pending' THEN 'widget'
    END,
    'observed_at',CASE amo_license.state
        WHEN 'paid' THEN l.observed_at
        WHEN 'unpaid' THEN CASE WHEN payment.blocked_at=signals.widget_blocked_at THEN pay.observed_at ELSE payment.blocked_at END
        WHEN 'trial' THEN signals.trial_observed_at
        WHEN 'expired_pending' THEN signals.trial_observed_at
    END,
    'trial_end',CASE WHEN amo_license.state IN ('trial','expired_pending') THEN signals.trial_end END))`

func enrollAmoStart(ctx context.Context, tx pgx.Tx) (bool, error) {
	var busy bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
        SELECT FROM platform_jobs.jobs WHERE kind='accounts.refresh' AND status IN ('queued','running') AND payload->>'component'='amostart'
    ) OR EXISTS (
        SELECT FROM platform_manager.account_refresh_slots WHERE component='amostart'
        AND attempted_at>statement_timestamp()-interval '45 seconds'
    )`).Scan(&busy); err != nil || busy {
		return false, err
	}
	var account uuid.UUID
	err := tx.QueryRow(ctx, `
        WITH candidates AS (
            SELECT account_id FROM platform_manager.account_tariff_fields WHERE field='pay_type' AND trial_or_blocked_at IS NOT NULL
            UNION SELECT account_id FROM platform_manager.account_refresh_slots WHERE component='license' AND payment_required_at IS NOT NULL
        )
        SELECT a.account_id FROM candidates c JOIN platform_manager.visible_accounts a USING(account_id)
        LEFT JOIN platform_manager.account_identities i USING(account_id)
        LEFT JOIN platform_manager.account_amostart_licenses l USING(account_id)
        LEFT JOIN platform_manager.account_refresh_slots ls ON ls.account_id=a.account_id AND ls.component='amostart'
        `+accountLicenseEvidenceJoins+`
        CROSS JOIN (SELECT statement_timestamp() AS as_of) license_clock
        `+accountLicenseStatusJoin+`
        WHERE (ls.next_check_at IS NULL OR ls.next_check_at<=license_clock.as_of)
          AND amo_license.state<>'paid'
          AND (signals.trial_end<=license_clock.as_of
            OR (payment.blocked_at IS NOT NULL AND (signals.trial_at IS NULL OR payment.blocked_at>=signals.trial_at))
            OR (signals.trial_at IS NULL AND payment.paid_floor_at IS NOT NULL))
          AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p WHERE p.account_id=a.account_id AND p.classification='Запрет работы')
        ORDER BY ls.next_check_at NULLS FIRST,a.account_id LIMIT 1`).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := lockRefreshSlot(ctx, tx, account, refreshAmoStart); err != nil {
		return false, err
	}
	var due bool
	if err := tx.QueryRow(ctx, `SELECT next_check_at<=statement_timestamp() FROM platform_manager.account_refresh_slots
        WHERE account_id=$1 AND component='amostart'`, account).Scan(&due); err != nil || !due {
		return false, err
	}
	_, err = joinAccountRefresh(ctx, tx, account, refreshAmoStart, sourceScope{})
	return err == nil, err
}
