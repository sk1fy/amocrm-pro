package productplatform

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func BootstrapLifecycle(ctx context.Context, owner *pgxpool.Pool, raw []byte) error {
	manifest, digest, err := wire.DecodeManifest(raw)
	if err != nil {
		return err
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.manifest(version,digest) VALUES($1,$2) ON CONFLICT DO NOTHING`,
		manifest.Version, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var installedDigest string
		err := tx.QueryRow(ctx, `SELECT digest FROM platform_lifecycle.manifest WHERE version=$1`,
			manifest.Version).Scan(&installedDigest)
		if err != nil {
			return err
		}
		if installedDigest != digest {
			return errors.New("Platform pilot manifest differs from installed version")
		}
		return tx.Commit(ctx)
	}
	for _, mapping := range manifest.Integrations {
		if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle.products(id, code) VALUES($1,$2) ON CONFLICT DO NOTHING`,
			uuid.New(), mapping.ProductCode); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO platform_lifecycle.integrations(id,product_id)
			SELECT $1,id FROM platform_lifecycle.products WHERE code=$2`, mapping.Integration, mapping.ProductCode); err != nil {
			return err
		}
		for _, rawAccount := range mapping.Accounts {
			account, _ := wire.AccountID(rawAccount)
			if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,$2)`,
				mapping.Integration, account); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
