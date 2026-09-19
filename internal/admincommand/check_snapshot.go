package admincommand

import (
	"context"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"time"
)

func persistCheck(ctx context.Context, tx jobs.TxExecutor, job jobs.Job, r executionResult) (bool, error) {
	// Lock order matches OAuth: installation, credentials, then projection. Job
	// completion and the projection become visible in the same transaction.
	if _, err := tx.Exec(ctx, `SELECT id FROM installations WHERE id=$1 FOR UPDATE`, job.InstallationID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `SELECT installation_id FROM oauth_credentials WHERE installation_id=$1 FOR UPDATE`, job.InstallationID); err != nil {
		return false, err
	}
	interval := time.Duration(r.CheckIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	tag, err := tx.Exec(ctx, `INSERT INTO installation_checks(installation_id,receipt_id,credential_version,installation_status,classification,observed_at,retry_after,failures,next_check_at)
 SELECT i.id,c.id,$2,i.status,$3,$4,$5,CASE WHEN $3 IN ('verified_ok','auth_error') THEN 0 ELSE 1 END,$4::timestamptz + make_interval(secs => $6)
 FROM installations i JOIN integrations ig ON ig.id=i.integration_id
 JOIN admin_commands c ON c.job_id=$1
 LEFT JOIN oauth_credentials oc ON oc.installation_id=i.id
 WHERE i.id=c.installation_id AND COALESCE(oc.token_version,0)=$2
 AND ig.status='active' AND (i.status=$7 OR ($3='auth_error' AND i.status='reauth_required'))
 AND i.status NOT IN ('disabled','uninstalled')
 ON CONFLICT(installation_id) DO UPDATE SET receipt_id=excluded.receipt_id,
 credential_version=excluded.credential_version,installation_status=excluded.installation_status,
 classification=excluded.classification,observed_at=excluded.observed_at,retry_after=excluded.retry_after,
 failures=CASE WHEN excluded.classification IN ('verified_ok','auth_error') THEN 0 ELSE installation_checks.failures+1 END,
 next_check_at=excluded.observed_at + make_interval(secs => greatest($6,CASE WHEN excluded.classification IN ('network_error','internal_error') THEN least(21600,$6*power(2,least(installation_checks.failures,3))) ELSE 0 END))
 WHERE installation_checks.observed_at <= excluded.observed_at`, job.ID, r.CheckVersion, r.Outcome, r.CheckObserved, r.RetryAfter, checkDelay(job.InstallationID.String(), r.CheckObserved, interval, r.RetryAfter).Seconds(), r.CheckStatus)
	return tag.RowsAffected() == 1, err
}
