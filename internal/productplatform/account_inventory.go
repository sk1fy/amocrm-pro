package productplatform

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
)

type inventoryEntry struct {
	installation, integration uuid.UUID
	product, authorization    string
	current, observationOnly  bool
	registered                time.Time
	sequence                  int64
}

func (o *AccountObserver) readInventory(ctx context.Context, account uuid.UUID) ([]inventoryEntry, queue.Outcome) {
	var amo int64
	if o.directory == nil {
		return nil, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
	if err := o.pool.QueryRow(ctx, `SELECT amo_account_id FROM platform_manager.visible_accounts WHERE account_id=$1`, account).Scan(&amo); err != nil {
		return nil, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: !errors.Is(err, pgx.ErrNoRows)}
	}
	var items []inventoryEntry
	var cursor string
	for {
		page, err := o.directory.ListInstallations(ctx, &platformpb.AccountDirectoryRequest{AmoAccountId: amo, AfterInstallationId: cursor})
		if err != nil {
			return nil, accountGatewayOutcome(err)
		}
		if page == nil || len(page.Entries) > 100 {
			return nil, queue.Outcome{FailureCode: "source_incomplete", Retry: true}
		}
		last := cursor
		for _, entry := range page.Entries {
			id, e1 := uuid.Parse(entry.GetInstallationId())
			integration, e2 := uuid.Parse(entry.GetIntegrationId())
			if e1 != nil || id == uuid.Nil || e2 != nil || integration == uuid.Nil || id.String() <= last || !entry.GetRegisteredAt().IsValid() || entry.GetProductCode() == "" {
				return nil, queue.Outcome{FailureCode: "source_incomplete", Retry: true}
			}
			switch entry.Authorization {
			case "unknown", "authorized", "disabled", "reauth_required":
			default:
				return nil, queue.Outcome{FailureCode: "source_incomplete", Retry: true}
			}
			items = append(items, inventoryEntry{id, integration, entry.ProductCode, entry.Authorization, entry.Current, entry.ObservationOnly, entry.RegisteredAt.AsTime(), entry.LifecycleSequence})
			last = id.String()
		}
		if page.NextCursor == "" {
			return items, queue.Outcome{}
		}
		if last == cursor || last != page.NextCursor || len(items) > 10000 {
			return nil, queue.Outcome{FailureCode: "source_incomplete", Retry: true}
		}
		cursor = last
	}
}

func publishInventory(ctx context.Context, tx pgx.Tx, account uuid.UUID, items []inventoryEntry) (queue.Outcome, error) {
	// Resolve every catalog code before replacing the prior complete observation.
	products := make(map[string]uuid.UUID)
	for _, item := range items {
		if _, ok := products[item.product]; ok {
			continue
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM platform_lifecycle.products WHERE code=$1 AND catalog_version IS NOT NULL`, item.product).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return queue.Outcome{FailureCode: "catalog_binding_missing"}, nil
		}
		if err != nil {
			return queue.Outcome{}, err
		}
		products[item.product] = id
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.installation)
		tag, err := tx.Exec(ctx, `INSERT INTO platform_manager.core_inventory AS stored
 (installation_id,integration_id,account_id,product_id,registered_at,observed_at,current,auth_state,observation_only,lifecycle_sequence)
 VALUES($1,$2,$3,$4,$5,statement_timestamp(),$6,$7,$8,$9)
 ON CONFLICT(installation_id) DO UPDATE SET observed_at=EXCLUDED.observed_at,current=EXCLUDED.current,
   auth_state=EXCLUDED.auth_state,observation_only=EXCLUDED.observation_only,lifecycle_sequence=EXCLUDED.lifecycle_sequence
 WHERE (stored.account_id,stored.product_id,stored.integration_id)=(EXCLUDED.account_id,EXCLUDED.product_id,EXCLUDED.integration_id)`, item.installation, item.integration, account, products[item.product], item.registered, item.current, item.authorization, item.observationOnly, item.sequence)
		if err != nil {
			return queue.Outcome{}, err
		}
		if tag.RowsAffected() != 1 {
			return queue.Outcome{}, errors.New("inventory installation changed account or product")
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM platform_manager.core_inventory WHERE account_id=$1 AND NOT (installation_id=ANY($2::uuid[]))`, account, ids); err != nil {
		return queue.Outcome{}, err
	}
	var missing bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.observed_core_installations o WHERE o.account_id=$1 AND NOT EXISTS(
    SELECT FROM platform_manager.core_inventory i WHERE i.installation_id=o.installation_id))`, account).Scan(&missing)
	if err != nil {
		return queue.Outcome{}, err
	}
	if missing {
		return queue.Outcome{FailureCode: "catalog_binding_missing"}, nil
	}
	return queue.Outcome{}, nil
}
