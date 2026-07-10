package widgetapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

func PingJobHandler(pool *pgxpool.Pool) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		if job.InstallationID == nil {
			return nil, jobs.Permanent("invalid_tenant_scope", errors.New("widget ping job has no installation"))
		}
		var payload struct {
			AccountID int64 `json:"account_id"`
			UserID    int64 `json:"user_id"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.AccountID <= 0 || payload.UserID <= 0 {
			return nil, jobs.Permanent("invalid_payload", errors.New("widget ping payload is invalid"))
		}
		metadata, _ := json.Marshal(map[string]int64{"account_id": payload.AccountID})
		if _, err := pool.Exec(ctx, `
			INSERT INTO audit_log (
				installation_id, actor_type, actor_id, action, object_type, object_id, metadata
			) VALUES ($1, 'widget_user', $2, 'widget.ping', 'job', $3, $4)`,
			*job.InstallationID, fmt.Sprint(payload.UserID), job.ID.String(), metadata,
		); err != nil {
			return nil, fmt.Errorf("audit widget ping: %w", err)
		}
		return json.RawMessage(`{"pong":true}`), nil
	}
}
