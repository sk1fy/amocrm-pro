package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type eventFilters struct {
	Route            string
	Account, Product *uuid.UUID
	Kinds            []string
	From, To, Zone   string
	Limit            int
	start, before    *time.Time
}

func (f eventFilters) hash() [32]byte {
	body, _ := json.Marshal(f)
	return sha256.Sum256(body)
}

type eventReference struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

type businessEvent struct {
	ID                 uuid.UUID        `json:"id"`
	Kind               string           `json:"kind"`
	OccurredAt         time.Time        `json:"occurred_at"`
	RecordedAt         time.Time        `json:"recorded_at"`
	Account            uuid.UUID        `json:"account_id"`
	Product            *uuid.UUID       `json:"product_id,omitempty"`
	AccountSubdomain   string           `json:"account_subdomain,omitempty"`
	ProductDisplayName string           `json:"product_display_name,omitempty"`
	Summary            string           `json:"summary"`
	Related            []eventReference `json:"related_refs"`
}

type eventRecord struct {
	businessEvent
	source  string
	episode *uuid.UUID
}

type eventPage struct {
	Items      []businessEvent `json:"items"`
	Next       *string         `json:"next_cursor"`
	SnapshotAt time.Time       `json:"snapshot_at"`
	QueryHash  string          `json:"query_hash"`
}

var eventSummaries = map[string]string{
	"installed": "Виджет установлен", "reinstalled": "Виджет переустановлен", "reauthorized": "Авторизация восстановлена", "uninstalled": "Виджет удалён",
	"installation_reviewed": "Установка проверена", "installation_unreviewed": "Проверка установки снята",
	"account_profile_updated": "Профиль аккаунта обновлён", "portrait_updated": "Портрет аккаунта обновлён", "access_renewed": "Доступ продлён",
	"activated": "Виджет активирован",
}

const eventVisible = `EXISTS (SELECT FROM platform_manager.visible_accounts a WHERE a.account_id=e.account_id)
 AND (e.product_id IS NULL OR EXISTS (SELECT FROM platform_manager.account_memberships membership
 WHERE membership.account_id=e.account_id AND membership.product_id=e.product_id))`
const eventColumns = `e.source,e.id,e.kind,e.occurred_at,e.recorded_at,e.account_id,e.product_id,e.episode_id`
const eventFrom = ` FROM platform_manager.business_events e WHERE ` + eventVisible + `
 AND ($1::uuid IS NULL OR e.account_id=$1) AND ($2::uuid IS NULL OR e.product_id=$2)
 AND (COALESCE(cardinality($3::text[]),0)=0 OR e.kind=ANY($3))
 AND ($4::timestamptz IS NULL OR e.occurred_at>=$4) AND ($5::timestamptz IS NULL OR e.occurred_at<$5)`

func collectEvents(rows pgx.Rows) ([]eventRecord, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (eventRecord, error) {
		var e eventRecord
		err := row.Scan(&e.source, &e.ID, &e.Kind, &e.OccurredAt, &e.RecordedAt, &e.Account, &e.Product, &e.episode)
		return e, err
	})
}

func publicEvents(ctx context.Context, tx pgx.Tx, records []eventRecord, actor managerActor) ([]businessEvent, error) {
	items := make([]businessEvent, len(records))
	for i, record := range records {
		e := record.businessEvent
		e.OccurredAt, e.RecordedAt = e.OccurredAt.UTC(), e.RecordedAt.UTC()
		e.Summary = eventSummaries[e.Kind]
		e.Related = []eventReference{}
		items[i] = e
	}
	if len(records) == 0 || !actor.Grants["clients.read"] {
		return items, nil
	}

	accounts, products := make([]uuid.UUID, len(records)), make([]*uuid.UUID, len(records))
	for i, record := range records {
		e := &items[i]
		accounts[i], products[i] = e.Account, e.Product
		e.Related = append(e.Related, eventReference{"account", e.Account})
		if record.episode != nil {
			e.Related = append(e.Related, eventReference{"installation", *record.episode})
		}
	}
	rows, err := tx.Query(ctx, `SELECT pair.ordinal, i.current_domain, p.display_name
 FROM unnest($1::uuid[], $2::uuid[]) WITH ORDINALITY AS pair(account_id,product_id,ordinal)
 LEFT JOIN platform_manager.account_identities i ON i.account_id=pair.account_id
 LEFT JOIN platform_lifecycle.products p ON p.id=pair.product_id AND p.catalog_version IS NOT NULL`, accounts, products)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var domain, productName *string
		if err := rows.Scan(&ordinal, &domain, &productName); err != nil {
			return nil, err
		}
		e := &items[ordinal-1]
		if domain != nil && validObservedDomain(*domain) {
			e.AccountSubdomain, _, _ = strings.Cut(*domain, ".")
		}
		if productName != nil {
			e.ProductDisplayName = *productName
			e.Related = append(e.Related, eventReference{"product", *e.Product})
		}
	}
	return items, rows.Err()
}

func (s managerStore) listEvents(ctx context.Context, actor managerActor, f eventFilters, after accountSeek) (eventPage, accountSeek, error) {
	hash := f.hash()
	page := eventPage{Items: []businessEvent{}, QueryHash: hex.EncodeToString(hash[:])}
	for range 3 {
		result, next, err := s.eventSnapshot(ctx, actor, f, after, page, hash)
		var dbErr *pgconn.PgError
		if !errors.As(err, &dbErr) || dbErr.Code != "40001" || ctx.Err() != nil {
			return result, next, err
		}
	}
	return page, accountSeek{}, errManagerSnapshotLimit
}

func (s managerStore) eventSnapshot(ctx context.Context, actor managerActor, f eventFilters, after accountSeek, page eventPage, hash [32]byte) (eventPage, accountSeek, error) {
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead}
	if after.Snapshot != uuid.Nil {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return page, accountSeek{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if f.Account != nil {
		var visible bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.visible_accounts WHERE account_id=$1)`, *f.Account).Scan(&visible)
		if err != nil {
			return page, accountSeek{}, err
		}
		if !visible {
			return page, accountSeek{}, errManagerNotFound
		}
	}
	if after.Snapshot != uuid.Nil {
		return continueEvents(ctx, tx, actor, f, after, page, hash)
	}
	if _, err = tx.Exec(ctx, `UPDATE platform_manager.account_snapshot_admission SET marker=NOT marker WHERE id`); err != nil {
		return page, accountSeek{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&page.SnapshotAt); err != nil {
		return page, accountSeek{}, err
	}
	rows, err := tx.Query(ctx, `SELECT `+eventColumns+eventFrom+` ORDER BY e.occurred_at DESC,e.recorded_at DESC,e.source,e.id LIMIT $6`, f.Account, f.Product, f.Kinds, f.start, f.before, accountSnapshotRows+1)
	if err != nil {
		return page, accountSeek{}, err
	}
	records, err := collectEvents(rows)
	if err != nil {
		return page, accountSeek{}, err
	}
	if len(records) > accountSnapshotRows {
		return page, accountSeek{}, errManagerQueryTooBroad
	}
	ids := make(map[uuid.UUID]bool, len(records))
	for _, e := range records {
		if ids[e.ID] {
			return page, accountSeek{}, errors.New("business event identity collision")
		}
		ids[e.ID] = true
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	page.Items, err = publicEvents(ctx, tx, records[:min(f.Limit, len(records))], actor)
	if err != nil {
		return page, accountSeek{}, err
	}
	if len(records) <= f.Limit {
		return page, accountSeek{}, tx.Commit(ctx)
	}
	snapshot := uuid.New()
	if err = saveEventSnapshot(ctx, tx, snapshot, actor, f, page.SnapshotAt, hash, len(records)); err != nil {
		return page, accountSeek{}, err
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"platform_manager", "feed_snapshot_items"}, []string{"snapshot_id", "ordinal", "source", "event_id"}, pgx.CopyFromSlice(len(records), func(i int) ([]any, error) {
		return []any{snapshot, i + 1, records[i].source, records[i].ID}, nil
	}))
	if err != nil {
		return page, accountSeek{}, err
	}
	return page, accountSeek{Snapshot: snapshot, Ordinal: f.Limit}, tx.Commit(ctx)
}

func saveEventSnapshot(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor managerActor, f eventFilters, at time.Time, hash [32]byte, total int) error {
	if _, err := tx.Exec(ctx, `DELETE FROM platform_manager.feed_snapshots WHERE expires_at<=transaction_timestamp()
 OR id IN (SELECT id FROM platform_manager.feed_snapshots WHERE subject_id=$1 ORDER BY expires_at DESC,id DESC OFFSET $2)`, actor.Subject, accountEmployeeSnapshots-1); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_manager.feed_snapshots`).Scan(&count); err != nil {
		return err
	}
	if count >= accountGlobalSnapshots {
		return errManagerSnapshotLimit
	}
	_, err := tx.Exec(ctx, `INSERT INTO platform_manager.feed_snapshots(id,route,account_id,subject_id,grants_version,query_hash,scope_hash,snapshot_at,expires_at,total)
 VALUES($1,$2,$3,$4,$5,$6,`+approvedScopeHash+`,$7,$8,$9)`, id, f.Route, f.Account, actor.Subject, actor.GrantsVersion, hash[:], at, at.Add(managerSnapshotTTL), total)
	return err
}

func continueEvents(ctx context.Context, tx pgx.Tx, actor managerActor, f eventFilters, after accountSeek, page eventPage, hash [32]byte) (eventPage, accountSeek, error) {
	var total int
	err := tx.QueryRow(ctx, `SELECT snapshot_at,total FROM platform_manager.feed_snapshots WHERE id=$1 AND route=$2
 AND account_id IS NOT DISTINCT FROM $3::uuid AND subject_id=$4 AND grants_version=$5 AND query_hash=$6
 AND expires_at>statement_timestamp() AND scope_hash=`+approvedScopeHash, after.Snapshot, f.Route, f.Account, actor.Subject, actor.GrantsVersion, hash[:]).Scan(&page.SnapshotAt, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, accountSeek{}, errManagerCursor
	}
	if err != nil {
		return page, accountSeek{}, err
	}
	var visible int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM platform_manager.feed_snapshot_items item JOIN platform_manager.business_events e
 ON (e.source,e.id)=(item.source,item.event_id) WHERE item.snapshot_id=$1 AND `+eventVisible, after.Snapshot).Scan(&visible)
	if err != nil {
		return page, accountSeek{}, err
	}
	if visible != total || after.Ordinal >= total {
		return page, accountSeek{}, errManagerCursor
	}
	rows, err := tx.Query(ctx, `SELECT `+eventColumns+` FROM platform_manager.feed_snapshot_items item JOIN platform_manager.business_events e
 ON (e.source,e.id)=(item.source,item.event_id) WHERE item.snapshot_id=$1 AND item.ordinal>$2 AND `+eventVisible+` ORDER BY item.ordinal LIMIT $3`, after.Snapshot, after.Ordinal, f.Limit)
	if err != nil {
		return page, accountSeek{}, err
	}
	records, err := collectEvents(rows)
	if err != nil {
		return page, accountSeek{}, err
	}
	page.Items, err = publicEvents(ctx, tx, records, actor)
	if err != nil {
		return page, accountSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	after.Ordinal += len(records)
	if after.Ordinal == total {
		after = accountSeek{}
	}
	return page, after, tx.Commit(ctx)
}
