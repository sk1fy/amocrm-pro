package webhook

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

// EnqueueReconcileTx uses the existing worker handler; HTTP performs no amoCRM I/O.
func EnqueueReconcileTx(ctx context.Context, tx pgx.Tx, store *jobs.Store, id uuid.UUID, actor string) (jobs.Job, error) {
	var status, integrationStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM integrations WHERE id=(SELECT integration_id FROM installations WHERE id=$1) FOR SHARE`, id).Scan(&integrationStatus); err != nil {
		return jobs.Job{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM installations WHERE id=$1 FOR SHARE`, id).Scan(&status); err != nil {
		return jobs.Job{}, err
	}
	if status != "active" || integrationStatus != "active" {
		return jobs.Job{}, ErrNotActive
	}
	job, err := store.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &id, Type: "webhook.reconcile", ActorType: "admin", ActorID: actor,
		Payload: map[string]string{"installation_id": id.String()},
	})
	if err != nil {
		return jobs.Job{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log(installation_id,actor_type,actor_id,action,object_type,object_id)
		VALUES($1,'admin',$2,'webhook.reconcile.requested','job',$3)`, id, actor, job.ID.String()); err != nil {
		return jobs.Job{}, errors.New("audit reconcile admission failed")
	}
	return job, nil
}
