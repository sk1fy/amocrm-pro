package distribution

import (
	"context"
	"log/slog"
	"time"

	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/transport/httpmiddleware"
	"golang.org/x/sync/errgroup"
)

type referenceSources struct {
	timezone          string
	timezoneFetchedAt time.Time
	users             []amocrm.DistributionUser
	pipelines         []amocrm.Pipeline
}

// These independent reads share the same CRM limiter and request deadline.
// Caller retains both capability fences; no rights or reference cache is added.
func readReferenceSources(ctx context.Context, crm CRM, b Binding) (referenceSources, error) {
	var out referenceSources
	group, callCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		zone, err := crm.DistributionAccountTimezone(callCtx, b.InstallationID, b.AccountID)
		if err == nil {
			if zone == "" || zone == "Local" || len(zone) > 128 {
				err = amocrm.ErrIncompleteResponse
			} else if _, parseErr := time.LoadLocation(zone); parseErr != nil {
				err = amocrm.ErrIncompleteResponse
			}
		}
		if err != nil {
			logSourceUnavailable(ctx, "crm_timezone", err)
			return err
		}
		out.timezone, out.timezoneFetchedAt = zone, time.Now().UTC()
		return nil
	})
	group.Go(func() error {
		users, err := crm.DistributionUsers(callCtx, b.InstallationID)
		if err != nil {
			logSourceUnavailable(ctx, "crm_users", err)
			return err
		}
		out.users = users
		return nil
	})
	group.Go(func() error {
		pipelines, err := crm.DistributionPipelines(callCtx, b.InstallationID)
		if err != nil {
			logSourceUnavailable(ctx, "crm_pipelines", err)
			return err
		}
		out.pipelines = pipelines
		return nil
	})
	if err := group.Wait(); err != nil {
		return referenceSources{}, err
	}
	return out, nil
}

func logSourceUnavailable(ctx context.Context, stage string, err error) {
	reason, status := amocrm.FailureDiagnostic(err)
	slog.WarnContext(ctx, "distribution source unavailable",
		"request_id", httpmiddleware.RequestIDFromContext(ctx).String(),
		"stage", stage, "reason_code", reason, "upstream_status", status)
}
