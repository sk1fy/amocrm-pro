package productplatform

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

const dealTariffResumeExhausted = "deal_tariff_resume_exhausted"

func (d InstallationDeals) deferForTariff(episode uuid.UUID) queue.Completion {
	return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
		// Completion already holds the job lock. An observation that committed
		// first is visible here, and one still waiting will inspect the parked deal.
		var technical, eligible, fresh bool
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(t.numeric_value=$2,false),
				inst.lifecycle='current' AND p.classification IS DISTINCT FROM 'Запрет работы',
				COALESCE(t.observed_at>=inst.opened_at,false)
			FROM platform_manager.installations inst
			LEFT JOIN platform_manager.account_profiles p USING(account_id)
			LEFT JOIN platform_manager.account_tariff_fields t ON t.account_id=inst.account_id AND t.field='tariff_id'
			WHERE inst.id=$1`, episode, technicalAccountTariffID).Scan(&technical, &eligible, &fresh)
		if err != nil {
			return queue.Outcome{}, err
		}
		if technical {
			return d.publish(episode, dealTechnicalAccount, companycrm.DealResult{}, queue.Outcome{}).Publish(ctx, tx)
		}
		if !eligible {
			return d.publish(episode, "skipped", companycrm.DealResult{}, queue.Outcome{}).Publish(ctx, tx)
		}
		if fresh {
			return queue.Outcome{Reschedule: true, FailureCode: dealTariffResumeExhausted}, nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE platform_manager.installation_deals SET reason=$2
			WHERE episode_id=$1 AND status='pending' AND title IS NULL`, episode, dealTariffUnknown)
		return queue.Outcome{}, err
	}}
}

func lockUnreservedInstallationDealJobs(ctx context.Context, tx pgx.Tx, account uuid.UUID) ([]uuid.UUID, error) {
	// Lock running jobs too. Completion may park one while this statement
	// waits, so the caller must inspect the reason in a new statement.
	rows, err := tx.Query(ctx, `
		SELECT j.id FROM platform_lifecycle.account_products p
		JOIN platform_lifecycle.episodes e ON e.installation_id=p.installation_id AND e.generation=1
		JOIN platform_manager.installation_deals d ON d.episode_id=e.id
		JOIN platform_jobs.jobs j ON j.id=e.id AND j.kind='installation.deal'
		WHERE p.account_id=$1 AND d.status='pending' AND d.title IS NULL
		ORDER BY j.id FOR UPDATE OF j`, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func wakeInstallationDealsForTariff(ctx context.Context, tx pgx.Tx, account uuid.UUID) error {
	jobs, err := lockUnreservedInstallationDealJobs(ctx, tx, account)
	if err != nil || len(jobs) == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH resumed AS (
			UPDATE platform_manager.installation_deals d SET reason=NULL
			FROM platform_jobs.jobs j, platform_lifecycle.episodes e, platform_manager.account_tariff_fields t
			WHERE d.episode_id=ANY($1) AND j.id=d.episode_id AND j.status='succeeded'
				AND d.status='pending' AND d.title IS NULL AND d.reason=$2
				AND e.id=d.episode_id AND t.account_id=$3 AND t.field='tariff_id' AND t.observed_at>=e.opened_at
			RETURNING d.episode_id
		)
		UPDATE platform_jobs.jobs j
		SET status=CASE WHEN j.attempt<$4 THEN 'queued' ELSE 'dead' END,
			max_attempts=GREATEST(j.max_attempts,LEAST(j.attempt+1,$4)),
			available_at=statement_timestamp(),updated_at=statement_timestamp(),
			failure_code=CASE WHEN j.attempt>=$4 THEN $5 END
		FROM resumed WHERE j.id=resumed.episode_id`, jobs, dealTariffUnknown, account, queue.MaxJobAttempts, dealTariffResumeExhausted)
	return err
}

func skipRemovedInstallationDealWaitingForTariff(ctx context.Context, tx pgx.Tx, episode uuid.UUID) error {
	// Wait for concurrent completion before checking whether it parked the job.
	if _, err := tx.Exec(ctx, `SELECT id FROM platform_jobs.jobs WHERE id=$1 AND kind='installation.deal' FOR UPDATE`, episode); err != nil {
		return err
	}
	return skipInstallationDealsWaitingForTariff(ctx, tx, []uuid.UUID{episode})
}

func skipParkedInstallationDealsForBan(ctx context.Context, tx pgx.Tx, account uuid.UUID) error {
	jobs, err := lockUnreservedInstallationDealJobs(ctx, tx, account)
	if err != nil || len(jobs) == 0 {
		return err
	}
	return skipInstallationDealsWaitingForTariff(ctx, tx, jobs)
}

func skipInstallationDealsWaitingForTariff(ctx context.Context, tx pgx.Tx, jobs []uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		UPDATE platform_manager.installation_deals d SET status='skipped',reason=NULL
		FROM platform_jobs.jobs j
		WHERE d.episode_id=ANY($1) AND j.id=d.episode_id AND j.status='succeeded'
			AND d.status='pending' AND d.title IS NULL AND d.reason=$2`, jobs, dealTariffUnknown)
	return err
}
