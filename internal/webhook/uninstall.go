package webhook

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/corelifecycle"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
)

type UninstallGateway interface {
	Gateway
	ListWebhooksForUninstall(context.Context, uuid.UUID) ([]amocrm.Webhook, error)
}

func (s *ReconcileStore) withUninstall(ctx context.Context, id, attempt uuid.UUID, action func(pgx.Tx, corelifecycle.Installation) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	i, err := corelifecycle.LockInstallation(ctx, tx, id)
	if err != nil {
		return false, err
	}
	confirmed, err := corelifecycle.CheckUninstall(ctx, tx, i, attempt)
	if err != nil || confirmed {
		return confirmed, err
	}
	if err := action(tx, i); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func UnregisterAttempt(ctx context.Context, store *ReconcileStore, client UninstallGateway, id, attempt uuid.UUID) error {
	if attempt == uuid.Nil {
		_, err := store.withUninstall(ctx, id, attempt, func(pgx.Tx, corelifecycle.Installation) error { return nil })
		if err != nil {
			return err
		}
		return Unregister(ctx, store, client, id)
	}

	var sub subscription
	var owned []string
	confirmed, err := store.withUninstall(ctx, id, attempt, func(tx pgx.Tx, _ corelifecycle.Installation) error {
		if err := requireNoInFlightReconcile(ctx, tx, id); err != nil {
			return err
		}
		var err error
		sub, err = store.loadSubscription(ctx, tx, id)
		if err != nil {
			return err
		}
		owned, err = store.readOwnedDestinations(ctx, tx, id)
		if err != nil {
			return err
		}
		var hasCredentials bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM oauth_credentials WHERE installation_id=$1)`,
			id).Scan(&hasCredentials); err != nil {
			return err
		}
		if !hasCredentials || sub.WebhookKey == "" {
			return errors.New("uninstall ownership or credentials require reconciliation")
		}
		return nil
	})
	if err != nil || confirmed {
		return err
	}

	pending := func() (bool, error) {
		done, err := store.withUninstall(ctx, id, attempt, func(pgx.Tx, corelifecycle.Installation) error { return nil })
		return !done, err
	}
	targets := func(actual []amocrm.Webhook) []string {
		ownedNow := append([]string(nil), owned...)
		for _, hook := range actual {
			if hasInstallationWebhookKey(hook.Destination, sub.WebhookKey) {
				ownedNow = append(ownedNow, hook.Destination)
			}
		}
		return planManagedWebhooks("", nil, actual, true, ownedNow...).delete
	}

	actual, err := client.ListWebhooksForUninstall(ctx, id)
	if err != nil {
		return classifyReconcileError(err)
	}
	deletions := targets(actual)
	if len(deletions) > 0 {
		confirmed, err := store.withUninstall(ctx, id, attempt, func(tx pgx.Tx, _ corelifecycle.Installation) error {
			for _, destination := range deletions {
				if slices.Contains(owned, destination) {
					continue
				}
				if err := store.writeOwnedDestination(ctx, tx, id, destination); err != nil {
					return err
				}
				owned = append(owned, destination)
			}
			return nil
		})
		if err != nil || confirmed {
			return err
		}
	}
	for _, destination := range deletions {
		if current, err := pending(); err != nil || !current {
			return err
		}
		if err := deleteWebhook(ctx, client, id, destination); err != nil {
			return classifyReconcileError(err)
		}
	}
	if len(deletions) > 0 {
		if current, err := pending(); err != nil || !current {
			return err
		}
		remaining, err := client.ListWebhooksForUninstall(ctx, id)
		if err != nil {
			return classifyReconcileError(err)
		}
		if len(targets(remaining)) != 0 {
			return errors.New("owned webhook removal requires reconciliation")
		}
	}

	_, err = store.withUninstall(ctx, id, attempt, func(tx pgx.Tx, i corelifecycle.Installation) error {
		if err := requireNoInFlightReconcile(ctx, tx, id); err != nil {
			return err
		}
		for _, destination := range owned {
			hash := sha256.Sum256([]byte(destination))
			if _, err := tx.Exec(ctx, `
				DELETE FROM installation_webhook_destinations WHERE installation_id=$1 AND destination_hash=$2`, id, hash[:]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE installations SET webhook_status='unregistered',webhook_checked_at=now(),webhook_last_error=NULL WHERE id=$1`, id); err != nil {
			return err
		}
		return corelifecycle.NewStore(store.pool).FinishUninstall(ctx, tx, i, attempt)
	})
	return err
}

// Processing jobs were able to observe the installation before it became
// uninstalled. Queued jobs have to reload its status before touching amoCRM.
func requireNoInFlightReconcile(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var processing bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM jobs WHERE installation_id=$1 AND type='webhook.reconcile' AND status='processing')`,
		id).Scan(&processing)
	if err != nil {
		return err
	}
	if processing {
		return errors.New("webhook registration is still in flight; retry cleanup after reconciliation")
	}
	return nil
}
