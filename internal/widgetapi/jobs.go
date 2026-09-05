package widgetapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	"strconv"
)

func PingJobHandler(store *ExecutionStore) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if err := store.Authorize(ctx, job, PingJobType, ""); err != nil {
			return nil, jobs.Permanent("action_not_authorized", err)
		}
		userID, err := JobActorUserID(job)
		if err != nil {
			return nil, jobs.Permanent("invalid_actor", err)
		}
		metadata, _ := json.Marshal(map[string]string{"job_id": job.ID.String()})
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO audit_log (
				installation_id, actor_type, actor_id, action, object_type, object_id,
				correlation_job_id, metadata
			) VALUES ($1, 'widget_user', $2, 'widget.ping', 'job', $3, $4, $5)
			ON CONFLICT (correlation_job_id) DO NOTHING`,
			*job.InstallationID, strconv.FormatInt(userID, 10), job.ID.String(), job.ID, metadata,
		); err != nil {
			return nil, fmt.Errorf("audit widget ping: %w", err)
		}
		return json.RawMessage(`{"pong":true}`), nil
	}
}
