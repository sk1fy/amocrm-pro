package webhook

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"
)

func destinationAAD(id uuid.UUID, hash []byte) []byte {
	return []byte(fmt.Sprintf("installation:%s:webhook-destination:%x", id, hash))
}

func (s *ReconcileStore) rememberDestination(ctx context.Context, id uuid.UUID, destination string) error {
	hash := sha256.Sum256([]byte(destination))
	sealed, version, err := s.keys.Seal([]byte(destination), destinationAAD(id, hash[:]))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO installation_webhook_destinations(installation_id,destination_hash,destination_ciphertext,key_version) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, hash[:], sealed, version)
	return err
}

func (s *ReconcileStore) ownedDestinations(ctx context.Context, id uuid.UUID) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT destination_hash,destination_ciphertext,key_version FROM installation_webhook_destinations WHERE installation_id=$1 ORDER BY created_at,destination_hash LIMIT 102`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var destinations []string
	for rows.Next() {
		var hash, sealed []byte
		var version int
		if err := rows.Scan(&hash, &sealed, &version); err != nil {
			return nil, err
		}
		plain, err := s.keys.Open(version, sealed, destinationAAD(id, hash))
		if err != nil {
			return nil, err
		}
		destinations = append(destinations, string(plain))
		clear(plain)
	}
	if len(destinations) > 101 {
		return nil, errors.New("webhook ownership history exceeds reconciliation limit")
	}
	return destinations, rows.Err()
}

// Legacy registrations predate the registry. Only an exact installation secret
// in the canonical hook path proves ownership; a shared hostname does not.
func hasInstallationWebhookKey(destination, key string) bool {
	if key == "" {
		return false
	}
	u, err := url.Parse(destination)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.EscapedPath() == "/hooks/amocrm/v1/"+url.PathEscape(key)
}

func (s *ReconcileStore) synchronize(ctx context.Context, client Gateway, sub subscription, desired string, unregister bool) error {
	if desired != "" {
		if err := s.rememberDestination(ctx, sub.InstallationID, desired); err != nil {
			return err
		}
	}
	actual, err := client.ListWebhooks(ctx, sub.InstallationID, "")
	if err != nil {
		return err
	}
	for _, hook := range actual {
		if hasInstallationWebhookKey(hook.Destination, sub.WebhookKey) {
			if err := s.rememberDestination(ctx, sub.InstallationID, hook.Destination); err != nil {
				return err
			}
		}
	}
	owned, err := s.ownedDestinations(ctx, sub.InstallationID)
	if err != nil {
		return err
	}
	plan := planManagedWebhooks(desired, sub.Settings, actual, unregister, owned...)
	if err := applyWebhookPlan(ctx, client, sub.InstallationID, plan, sub.Settings, desired); err != nil {
		return err
	}
	// Remove only the snapshot we reconciled. A concurrent registration's
	// intent must not be forgotten when this older run finishes.
	for _, destination := range owned {
		if !unregister && destination == desired {
			continue
		}
		hash := sha256.Sum256([]byte(destination))
		if _, err := s.pool.Exec(ctx, `DELETE FROM installation_webhook_destinations WHERE installation_id=$1 AND destination_hash=$2`, sub.InstallationID, hash[:]); err != nil {
			return err
		}
	}
	return nil
}
