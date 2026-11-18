package corelifecycle

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func (s *Store) Bootstrap(ctx context.Context, raw []byte) error {
	manifest, digest, err := wire.DecodeManifest(raw)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle_manifest(version,digest) VALUES($1,$2) ON CONFLICT DO NOTHING`,
		manifest.Version, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var installedDigest string
		err := tx.QueryRow(ctx, `SELECT digest FROM platform_lifecycle_manifest WHERE version=$1`,
			manifest.Version).Scan(&installedDigest)
		if err != nil {
			return err
		}
		if installedDigest != digest {
			return errors.New("Core Platform pilot manifest differs from installed version")
		}
		return tx.Commit(ctx)
	}
	for _, mapping := range manifest.Integrations {
		var integrationStatus string
		if err := tx.QueryRow(ctx, `SELECT status FROM integrations WHERE id=$1 FOR UPDATE`, mapping.Integration).Scan(&integrationStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("Platform pilot integration does not exist in Core")
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle_integrations(integration_id,product_code) VALUES($1,$2)`,
			mapping.Integration, mapping.ProductCode); err != nil {
			return err
		}
		for _, rawAccount := range mapping.Accounts {
			account, _ := wire.AccountID(rawAccount)
			if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle_scope(integration_id,account_id) VALUES($1,$2)`,
				mapping.Integration, account); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO platform_lifecycle_reconciliation(installation_id,reason)
				SELECT id,$3 FROM installations WHERE integration_id=$1 AND account_id=$2`,
				mapping.Integration, account, historyUnproven); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
