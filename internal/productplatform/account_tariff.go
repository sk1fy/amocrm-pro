package productplatform

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const technicalAccountTariffID int64 = 22000000

func (s *LifecycleStore) RecordAccountLicense(ctx context.Context, input wire.AccountLicenseRequest) error {
	if err := input.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	scope := input.Scope
	c, err := lockStream(ctx, tx, scope.Installation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEventScope
	}
	if err != nil {
		return err
	}
	if c.generation != scope.Generation || c.authorizationSequence != scope.AuthorizationSequence || c.authorization != wire.Authorized {
		return ErrEventScope
	}
	var account uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT account_id FROM platform_manager.account_memberships
		WHERE installation_id=$1 AND integration_id=$2 AND amo_account_id=$3 AND current`,
		scope.Installation, scope.Integration, scope.Account).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEventScope
	}
	if err != nil {
		return err
	}
	if _, err := lockAccountClassification(ctx, tx, account); err != nil {
		return err
	}
	if input.Hints.PayType != nil {
		if err := storeAccountPayType(ctx, tx, account, input); err != nil {
			return err
		}
	}
	var wasTechnical bool
	if input.Hints.TariffID != nil {
		wasTechnical, err = accountHasTechnicalTariff(ctx, tx, account)
		if err != nil {
			return err
		}
	}

	var tariffUpdated bool
	const newerNumericHint = `EXCLUDED.observed_at>latest.observed_at`
	err = tx.QueryRow(ctx, `
		WITH changed AS (
			INSERT INTO platform_manager.account_tariff_fields AS latest
				(account_id,field,numeric_value,installation_id,generation,authorization_sequence,observed_at,value_changed_at)
			SELECT $1,hint.field,hint.value,$2,$3,$4,$5,$5
			FROM (VALUES ('tariff_id',$6::bigint),('trial_end',$7::bigint)) hint(field,value)
			WHERE hint.value IS NOT NULL AND (hint.field <> 'trial_end' OR NOT EXISTS (
				SELECT FROM platform_manager.account_tariff_fields
				WHERE account_id=$1 AND field='pay_type' AND trial_reset_at >= $5))
			ON CONFLICT(account_id,field) DO UPDATE SET
				numeric_value=CASE WHEN `+newerNumericHint+` THEN EXCLUDED.numeric_value ELSE latest.numeric_value END,
				installation_id=CASE WHEN `+newerNumericHint+` THEN EXCLUDED.installation_id ELSE latest.installation_id END,
				generation=CASE WHEN `+newerNumericHint+` THEN EXCLUDED.generation ELSE latest.generation END,
				authorization_sequence=CASE WHEN `+newerNumericHint+` THEN EXCLUDED.authorization_sequence ELSE latest.authorization_sequence END,
				observed_at=GREATEST(EXCLUDED.observed_at,latest.observed_at),
				value_changed_at=CASE WHEN latest.numeric_value<>EXCLUDED.numeric_value
					THEN GREATEST(EXCLUDED.observed_at,latest.observed_at) ELSE latest.value_changed_at END
			WHERE `+newerNumericHint+` OR (latest.field='trial_end' AND latest.numeric_value<>EXCLUDED.numeric_value
				AND EXCLUDED.observed_at>latest.value_changed_at AND EXCLUDED.observed_at<latest.observed_at)
			RETURNING field
		)
		SELECT EXISTS(SELECT FROM changed WHERE field='tariff_id')`, account, scope.Installation, scope.Generation,
		scope.AuthorizationSequence, input.ReceivedAt, input.Hints.TariffID, input.Hints.TrialEnd).Scan(&tariffUpdated)
	if err != nil {
		return err
	}
	if tariffUpdated {
		tariffID := *input.Hints.TariffID
		isTechnical := tariffID == technicalAccountTariffID
		if wasTechnical != isTechnical {
			if err := applyTechnicalAccountClassification(ctx, tx, account, tariffID, input); err != nil {
				return err
			}
		}
		if isTechnical {
			err = skipTechnicalInstallationDeals(ctx, tx, account)
		} else {
			err = wakeInstallationDealsForTariff(ctx, tx, account)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func storeAccountPayType(ctx context.Context, tx pgx.Tx, account uuid.UUID, input wire.AccountLicenseRequest) error {
	// A delayed different type can reveal a transition between two observations
	// with the same type. Keep the newest observed type and retain the earlier signal.
	const changedType = `lower(EXCLUDED.text_value)<>lower(latest.text_value)`
	const recentTransition = changedType + ` AND EXCLUDED.observed_at>latest.value_changed_at`
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.account_tariff_fields AS latest
			(account_id,field,text_value,installation_id,generation,authorization_sequence,observed_at,value_changed_at,trial_reset_at,trial_or_blocked_at)
		VALUES($1,'pay_type',$2,$3,$4,$5,$6::timestamptz,$6::timestamptz,CASE WHEN lower($2)<>'trial' THEN $6::timestamptz END,
			CASE WHEN lower($2) IN ('trial','blocked') THEN $6::timestamptz END)
		ON CONFLICT(account_id,field) DO UPDATE SET
			text_value=CASE WHEN EXCLUDED.observed_at>latest.observed_at THEN EXCLUDED.text_value ELSE latest.text_value END,
			installation_id=CASE WHEN EXCLUDED.observed_at>latest.observed_at THEN EXCLUDED.installation_id ELSE latest.installation_id END,
			generation=CASE WHEN EXCLUDED.observed_at>latest.observed_at THEN EXCLUDED.generation ELSE latest.generation END,
			authorization_sequence=CASE WHEN EXCLUDED.observed_at>latest.observed_at THEN EXCLUDED.authorization_sequence ELSE latest.authorization_sequence END,
			observed_at=GREATEST(EXCLUDED.observed_at,latest.observed_at),
			value_changed_at=CASE WHEN `+recentTransition+`
				THEN GREATEST(EXCLUDED.observed_at,latest.observed_at) ELSE latest.value_changed_at END,
			trial_reset_at=GREATEST(EXCLUDED.trial_reset_at,latest.trial_reset_at),
			trial_or_blocked_at=GREATEST(latest.trial_or_blocked_at,
				CASE WHEN `+changedType+` THEN EXCLUDED.trial_or_blocked_at END,
				CASE WHEN `+recentTransition+` AND EXCLUDED.observed_at<latest.observed_at
					AND lower(latest.text_value) IN ('trial','blocked') THEN latest.observed_at END)
		WHERE EXCLUDED.observed_at>latest.observed_at OR (EXCLUDED.observed_at<latest.observed_at
			AND (GREATEST(EXCLUDED.trial_reset_at,latest.trial_reset_at) IS DISTINCT FROM latest.trial_reset_at
			  OR (`+changedType+` AND (EXCLUDED.observed_at>latest.value_changed_at
				  OR GREATEST(EXCLUDED.trial_or_blocked_at,latest.trial_or_blocked_at) IS DISTINCT FROM latest.trial_or_blocked_at))))`,
		account, input.Hints.PayType, input.Scope.Installation,
		input.Scope.Generation, input.Scope.AuthorizationSequence, input.ReceivedAt)
	return err
}

func lockAccountClassification(ctx context.Context, tx pgx.Tx, account uuid.UUID) (string, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.account_profiles(account_id) VALUES($1) ON CONFLICT DO NOTHING`, account); err != nil {
		return "", err
	}
	var classification string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(classification,'') FROM platform_manager.account_profiles WHERE account_id=$1 FOR UPDATE`, account).Scan(&classification)
	return classification, err
}

func skipTechnicalInstallationDeals(ctx context.Context, tx pgx.Tx, account uuid.UUID) error {
	jobs, err := lockUnreservedInstallationDealJobs(ctx, tx, account)
	if err != nil || len(jobs) == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH skipped AS (
			UPDATE platform_manager.installation_deals d SET status='skipped',reason=$2
			FROM platform_jobs.jobs j
			WHERE d.episode_id=ANY($1) AND j.id=d.episode_id AND d.status='pending' AND d.title IS NULL
			AND (j.status IN ('queued','running') OR (j.status='succeeded' AND d.reason=$3))
			RETURNING d.episode_id
		)
		UPDATE platform_jobs.jobs j SET status='succeeded',lease_until=NULL,failure_code=NULL,updated_at=statement_timestamp()
		FROM skipped WHERE j.id=skipped.episode_id`, jobs, dealTechnicalAccount, dealTariffUnknown)
	return err
}
