package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	productCreateCommandKind = "product.create"
	productUpdateCommandKind = "product.update"
)

type productTrial struct {
	Kind string `json:"kind"`
	Days *int   `json:"duration_days"`
}

type productLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type managerProduct struct {
	ID           uuid.UUID     `json:"id"`
	Code         string        `json:"code"`
	DisplayName  string        `json:"display_name"`
	Line         string        `json:"line"`
	Availability string        `json:"availability"`
	Integrations []uuid.UUID   `json:"integration_refs"`
	Tariff       *int64        `json:"tariff_version"`
	Trial        productTrial  `json:"trial_policy"`
	Links        []productLink `json:"links"`
	Version      int64         `json:"version"`
}

const productColumns = `
	p.id,p.code,p.display_name,p.line,p.availability,
	ARRAY(SELECT id FROM platform_lifecycle.integrations WHERE product_id=p.id ORDER BY id),
	p.tariff_version,r.rule,p.links,p.catalog_version`

const productFrom = `
	FROM platform_lifecycle.products p JOIN platform_lifecycle.product_trial_revisions r
	ON r.product_id=p.id AND r.version=p.catalog_policy_version`

func scanProduct(row pgx.Row) (managerProduct, error) {
	var p managerProduct
	var rule string
	err := row.Scan(&p.ID, &p.Code, &p.DisplayName, &p.Line, &p.Availability, &p.Integrations, &p.Tariff, &rule, &p.Links, &p.Version)
	p.Trial = trialForRule(rule)
	return p, err
}

func trialForRule(rule string) productTrial {
	policy := productTrial{Kind: "none"}
	switch rule {
	case "free":
		policy.Kind = "free"
	case "top_90d":
		days := 90
		policy.Kind, policy.Days = "once", &days
	case "pro_7d":
		days := 7
		policy.Kind, policy.Days = "once", &days
	case "pro_14d":
		days := 14
		policy.Kind, policy.Days = "once", &days
	}
	return policy
}

func (s managerStore) product(ctx context.Context, id uuid.UUID) (managerProduct, error) {
	p, err := scanProduct(s.pool.QueryRow(ctx, `SELECT `+productColumns+productFrom+` WHERE p.id=$1 AND p.catalog_version IS NOT NULL`, id))
	return p, accountReadError(err)
}

type productFilters struct {
	Q, Line, Availability string
	Limit                 int
}

func (f productFilters) hash() [32]byte {
	raw, _ := json.Marshal(f)
	return sha256.Sum256(raw)
}

type productPage struct {
	Items      []managerProduct `json:"items"`
	Next       *string          `json:"next_cursor"`
	SnapshotAt time.Time        `json:"snapshot_at"`
	QueryHash  string           `json:"query_hash"`
}

type productSeek struct {
	Revision   int64     `json:"revision"`
	Code       string    `json:"code"`
	ID         uuid.UUID `json:"id"`
	SnapshotAt time.Time `json:"snapshot_at"`
}

func (s managerStore) products(ctx context.Context, f productFilters, after productSeek) (productPage, productSeek, error) {
	hash := f.hash()
	page := productPage{QueryHash: hex.EncodeToString(hash[:])}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, productSeek{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var revision int64
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT version,transaction_timestamp() FROM platform_manager.product_catalog_revision WHERE id`).Scan(&revision, &now); err != nil {
		return page, productSeek{}, err
	}
	page.SnapshotAt = now.UTC()
	if after.ID != uuid.Nil {
		if after.Revision != revision || after.SnapshotAt.After(now) || !after.SnapshotAt.Add(managerSnapshotTTL).After(now) {
			return page, productSeek{}, errManagerCursor
		}
		page.SnapshotAt = after.SnapshotAt
	}
	rows, err := tx.Query(ctx, `SELECT `+productColumns+productFrom+`
		WHERE p.catalog_version IS NOT NULL
		  AND ($1='' OR strpos(lower(p.code),lower($1))>0 OR strpos(lower(p.display_name),lower($1))>0)
		  AND ($2='' OR p.line=$2) AND ($3='' OR p.availability=$3)
		  AND ($4='' OR (p.code,p.id)>($4,$5::uuid))
		ORDER BY p.code,p.id LIMIT $6`, f.Q, f.Line, f.Availability, after.Code, after.ID, f.Limit+1)
	if err != nil {
		return page, productSeek{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (managerProduct, error) { return scanProduct(row) })
	if err != nil {
		return page, productSeek{}, err
	}
	page.Items = items[:min(len(items), f.Limit)]
	var next productSeek
	if len(items) > f.Limit {
		last := page.Items[len(page.Items)-1]
		next = productSeek{Revision: revision, Code: last.Code, ID: last.ID, SnapshotAt: page.SnapshotAt}
	}
	return page, next, tx.Commit(ctx)
}

type productMetrics struct {
	Installations int64     `json:"confirmed_installations"`
	Activations   int64     `json:"activations"`
	ActiveAccess  int64     `json:"active_accesses"`
	SnapshotAt    time.Time `json:"snapshot_at"`
}

func (s managerStore) productMetrics(ctx context.Context, id uuid.UUID, start, before *time.Time) (productMetrics, error) {
	var m productMetrics
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM platform_manager.installations e WHERE e.product_id=p.id
				AND ($2::timestamptz IS NULL OR e.opened_at>=$2) AND ($3::timestamptz IS NULL OR e.opened_at<$3)),
			0::bigint,
			(SELECT count(*) FROM platform_manager.product_access access WHERE access.product_id=p.id AND access.authorized
				AND access.own_until>statement_timestamp() AND access.source IN ('own_trial','free','own_paid','manual')),
			statement_timestamp()
		FROM platform_lifecycle.products p WHERE p.id=$1 AND p.catalog_version IS NOT NULL`, id, start, before).
		Scan(&m.Installations, &m.Activations, &m.ActiveAccess, &m.SnapshotAt)
	return m, accountReadError(err)
}

func (s managerStore) productTarget(ctx context.Context, code string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM platform_lifecycle.products WHERE code=$1`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte("amocrm.platform.v1/products/"+code)), nil
	}
	return id, err
}
