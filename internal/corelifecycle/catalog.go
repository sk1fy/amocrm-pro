package corelifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type CatalogClient struct {
	endpoint string
	http     *http.Client
}

func NewCatalogClient(origin, identity string) (*CatalogClient, error) {
	u, h, err := platformClient(origin, identity, wire.CatalogPath)
	if err != nil {
		return nil, err
	}
	return &CatalogClient{u.String(), h}, nil
}

func (c *CatalogClient) Close() { c.http.CloseIdleConnections() }

func (c *CatalogClient) lookup(ctx context.Context, id uuid.UUID, registration *wire.IntegrationRegistration) (wire.CatalogPolicy, error) {
	method, path := http.MethodGet, c.endpoint+"?integration_id="+url.QueryEscape(id.String())
	var body []byte
	if registration != nil {
		method, path = http.MethodPost, c.endpoint
		body, _ = json.Marshal(registration)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return wire.CatalogPolicy{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return wire.CatalogPolicy{}, err
	}
	defer res.Body.Close()
	var p wire.CatalogPolicy
	raw, err := io.ReadAll(io.LimitReader(res.Body, wire.MaxBody+1))
	if err != nil || res.StatusCode != http.StatusOK || len(raw) > wire.MaxBody || wire.Decode(raw, &p) != nil || (p.Product != "" && !wire.ValidProductCode(p.Product)) {
		return p, errors.New("Platform catalog unavailable")
	}
	return p, nil
}

func (c *CatalogClient) Managed(ctx context.Context, id uuid.UUID) (bool, error) {
	p, err := c.lookup(ctx, id, nil)
	return p.Product != "", err
}

func (s *Store) SyncCatalog(ctx context.Context, c *CatalogClient, after map[uuid.UUID]uuid.UUID) error {
	rows, err := s.pool.Query(ctx, `SELECT id,status='active' FROM integrations ORDER BY id`)
	if err != nil {
		return err
	}
	registrations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (wire.IntegrationRegistration, error) {
		var r wire.IntegrationRegistration
		err := row.Scan(&r.ID, &r.Active)
		return r, err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, r := range registrations {
		if cursor, ok := after[r.ID]; ok {
			r.After = &cursor
		}
		p, err := c.lookup(ctx, r.ID, &r)
		if err == nil && p.Product != "" && r.Active {
			err = s.enrollCatalogProduct(ctx, r.ID, p.Product)
			if err == nil && len(p.Confirmed) > 0 {
				var ids []uuid.UUID
				var generations []int64
				for _, confirmed := range p.Confirmed {
					ids = append(ids, confirmed.ID)
					generations = append(generations, confirmed.Generation)
				}
				_, err = s.pool.Exec(ctx, `INSERT INTO widget_activation_confirmations AS existing(installation_id,generation)
                    SELECT c.installation_id,c.generation FROM unnest($2::uuid[],$3::bigint[]) AS confirmed(id,generation)
                    JOIN platform_lifecycle_cursors c ON c.installation_id=confirmed.id AND c.generation=confirmed.generation
                    JOIN installations i ON i.id=c.installation_id AND i.integration_id=$1
                    WHERE NOT c.closed
                    ON CONFLICT(installation_id) DO UPDATE SET generation=excluded.generation WHERE existing.generation<excluded.generation`, r.ID, ids, generations)
			}
			if err == nil && after != nil {
				if len(p.Confirmed) == wire.ConfirmationPageSize {
					after[r.ID] = p.Confirmed[len(p.Confirmed)-1].ID
				} else {
					delete(after, r.ID)
				}
			}
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Store) RunCatalogSync(ctx context.Context, c *CatalogClient, log *slog.Logger) {
	defer c.Close()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	after := make(map[uuid.UUID]uuid.UUID)
	for ctx.Err() == nil {
		if err := s.SyncCatalog(ctx, c, after); err != nil && ctx.Err() == nil {
			log.Error("widget catalog synchronization failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Store) enrollCatalogProduct(ctx context.Context, id uuid.UUID, code string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, "SELECT status FROM integrations WHERE id=$1 FOR UPDATE", id).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		return nil
	}
	var stored string
	err = tx.QueryRow(ctx, `SELECT product_code FROM platform_lifecycle_integrations WHERE integration_id=$1`, id).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if stored != "" && stored != code {
		return errors.New("catalog mapping conflicts with existing Core product")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_lifecycle_integrations AS m(integration_id,product_code,auto_enroll)
 VALUES($1,$2,true) ON CONFLICT(integration_id) DO UPDATE SET auto_enroll=true WHERE NOT m.auto_enroll`, id, code); err != nil {
		return err
	}

	// A historical first installation is provable only with the evidence from its
	// original OAuth transaction. Never turn a reauthorization into an install.
	rows, err := tx.Query(ctx, `SELECT i.id,i.account_id,i.created_at,COALESCE(i.installed_by,0),a.metadata->>'evidence_ref'
 FROM installations i JOIN audit_log a ON a.installation_id=i.id AND a.created_at=i.created_at
 AND a.actor_type='oauth' AND a.action='installation.authorized' AND a.metadata?'evidence_ref'
 WHERE i.integration_id=$1 AND i.status='active' AND EXISTS(SELECT FROM oauth_credentials o WHERE o.installation_id=i.id)
 AND NOT EXISTS(SELECT FROM platform_lifecycle_cursors c WHERE c.installation_id=i.id)
 AND NOT EXISTS(SELECT FROM platform_lifecycle_scope s JOIN platform_lifecycle_integrations m USING(integration_id)
                WHERE s.account_id=i.account_id AND m.product_code=$2 AND s.integration_id<>i.integration_id)
 AND NOT EXISTS(SELECT FROM audit_log b WHERE b.installation_id=i.id AND b.object_type='installation'
     AND (b.action='uninstall' OR b.metadata ? 'previous_status' OR
          (b.action='installation.authorized' AND (b.actor_type<>'oauth' OR NOT (b.metadata ? 'evidence_ref')))))
 ORDER BY i.account_id,i.id LIMIT 100 FOR UPDATE OF i`, id, code)
	if err != nil {
		return err
	}
	type original struct {
		ID        uuid.UUID
		Account   int64
		At        time.Time
		Installer int64
		Evidence  string
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowToStructByPos[original])
	if err != nil {
		return err
	}
	for _, i := range candidates {
		evidence, err := uuid.Parse(i.Evidence)
		if err != nil {
			return err
		}
		if err = enrollAccount(ctx, tx, id, i.Account); err != nil {
			return err
		}
		scoped, err := InScope(ctx, tx, Installation{Integration: id, Account: i.Account})
		if err != nil {
			return err
		}
		if !scoped {
			continue
		}
		if err = s.appendAt(ctx, tx, Installation{ID: i.ID, Account: i.Account, Integration: id, InstalledBy: i.Installer}, cursor{generation: 1}, wire.Installed, "", "core_oauth", evidence, "oauth_insert", i.At); err != nil {
			return err
		}
		later, err := tx.Query(ctx, `SELECT created_at,(metadata->>'evidence_ref')::uuid FROM audit_log WHERE installation_id=$1
            AND actor_type='oauth' AND action='installation.authorized' AND created_at>$2 ORDER BY created_at,id`, i.ID, i.At)
		if err != nil {
			return err
		}
		type renewal struct {
			At       time.Time
			Evidence uuid.UUID
		}
		renewals, err := pgx.CollectRows(later, pgx.RowToStructByPos[renewal])
		if err != nil {
			return err
		}
		for index, renewal := range renewals {
			prior := cursor{generation: 1, sequence: int64(index + 1)}
			if err = s.appendAt(ctx, tx, Installation{ID: i.ID, Account: i.Account, Integration: id}, prior, wire.Reauthorized, "", "core_oauth", renewal.Evidence, "oauth_upsert", renewal.At); err != nil {
				return err
			}
		}
		if err = resolve(ctx, tx, i.ID, historyUnproven); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
