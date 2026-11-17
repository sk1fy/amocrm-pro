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
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	accountSnapshotRows      = 40000
	accountEmployeeSnapshots = 2
	accountGlobalSnapshots   = 8
	crmUsersSource           = "core.gateway.amocrm.users"
)

type accountFilters struct {
	Q, Sort        string
	Classification []string
	Product        *uuid.UUID
	LicenseBefore  *time.Time
	UsersMin       *int
	UsersMax       *int
	Limit          int
}

func (f accountFilters) hash() [32]byte {
	body, _ := json.Marshal(f)
	return sha256.Sum256(body)
}

type accountPage struct {
	Items      []json.RawMessage `json:"items"`
	Next       *string           `json:"next_cursor"`
	SnapshotAt time.Time         `json:"snapshot_at"`
	QueryHash  string            `json:"query_hash"`
	Summary    accountSummary    `json:"summary"`
}

type accountSummary struct {
	Total int `json:"total"`
}

type accountSeek struct {
	Snapshot uuid.UUID
	Ordinal  int
}

const accountObservationJoins = `
	LEFT JOIN platform_manager.account_identities i USING(account_id)
	LEFT JOIN platform_manager.account_rosters u USING(account_id)
	LEFT JOIN platform_manager.account_profiles p USING(account_id)`

const rosterObservationState = `
	CASE WHEN u.observed_at IS NULL THEN 'unknown'
		WHEN u.failure_code IS NOT NULL OR u.fresh_until<=transaction_timestamp() THEN 'stale'
		ELSE 'fresh' END`

const rosterObservationFields = `
	'state',` + rosterObservationState + `,
	'source','` + crmUsersSource + `',
	'observed_at',u.observed_at,'fresh_until',u.fresh_until,'evidence_id',u.evidence_id,'error_code',u.failure_code`

// These two commercial observations have no verified Core source in V1.
const unsupportedAccountObservation = `jsonb_build_object('state','unsupported','source','unverified')`

const accountSummaryJSON = `jsonb_build_object(
	'account_id',a.account_id,'amo_account_id',a.amo_account_id::text,'current_domain',i.current_domain,
	'domain_aliases',(SELECT array_agg(d.domain ORDER BY d.domain) FROM platform_manager.account_domains d
		WHERE d.account_id=a.account_id AND d.domain<>i.current_domain),
	'display_name',COALESCE(p.display_name,i.display_name),'classification',p.classification,'version',COALESCE(p.version,1),
	'active_products_count',a.active_products_count,'license',` + unsupportedAccountObservation + `,
	'active_users',jsonb_strip_nulls(jsonb_build_object('value',u.active_count,` + rosterObservationFields + `)))`

func accountOrder(sort string) string {
	switch sort {
	case "current_domain":
		return "lower(i.current_domain) NULLS LAST, a.amo_account_id, a.account_id"
	case "display_name":
		return "lower(COALESCE(p.display_name,i.display_name)) NULLS LAST, a.amo_account_id, a.account_id"
	default:
		// License dates are unknown for every account, so both license sorts use the same tie breaker.
		return "a.amo_account_id, a.account_id"
	}
}

func (s managerStore) listAccounts(ctx context.Context, actor managerActor, f accountFilters, after accountSeek) (accountPage, accountSeek, error) {
	queryHash := f.hash()
	page := accountPage{QueryHash: hex.EncodeToString(queryHash[:])}
	for range 3 {
		var next accountSeek
		var err error
		page, next, err = s.accountSnapshot(ctx, actor, f, after, page, queryHash)
		var dbErr *pgconn.PgError
		if !errors.As(err, &dbErr) || dbErr.Code != "40001" || ctx.Err() != nil {
			return page, next, err
		}
	}
	return page, accountSeek{}, errManagerSnapshotLimit
}

func (s managerStore) accountSnapshot(ctx context.Context, actor managerActor, f accountFilters, after accountSeek,
	page accountPage, queryHash [32]byte) (accountPage, accountSeek, error) {
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead}
	if after.Snapshot != uuid.Nil {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return page, accountSeek{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if after.Snapshot != uuid.Nil {
		return continueAccounts(ctx, tx, actor, f, after, page, queryHash)
	}
	// A concurrent writer changes this row, making a stale repeatable read retry
	// before its admission count can exceed the database-wide storage bound.
	if _, err = tx.Exec(ctx, `UPDATE platform_manager.account_snapshot_admission SET marker=NOT marker WHERE id`); err != nil {
		return page, accountSeek{}, err
	}
	args := []any{f.Q, f.Classification, f.Product, f.LicenseBefore, f.UsersMin, f.UsersMax}
	err = tx.QueryRow(ctx, `
		SELECT transaction_timestamp(),count(*)::integer
		FROM (SELECT 1 `+accountListFrom+` LIMIT $7) matching`,
		append(args, accountSnapshotRows+1)...).Scan(&page.SnapshotAt, &page.Summary.Total)
	if err != nil {
		return page, accountSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	if page.Summary.Total > accountSnapshotRows {
		return page, accountSeek{}, errManagerSnapshotLimit
	}
	if page.Summary.Total <= f.Limit {
		rows, err := tx.Query(ctx, `SELECT `+accountSummaryJSON+accountListFrom+` ORDER BY `+accountOrder(f.Sort)+` LIMIT $7`,
			append(args, f.Limit)...)
		if err != nil {
			return page, accountSeek{}, err
		}
		page.Items, err = pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
		if err != nil {
			return page, accountSeek{}, err
		}
		return page, accountSeek{}, tx.Commit(ctx)
	}
	snapshot := uuid.New()
	if err = saveAccountSnapshot(ctx, tx, snapshot, actor, page, queryHash); err != nil {
		return page, accountSeek{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO platform_manager.account_snapshot_items(snapshot_id,ordinal,item)
		SELECT $7,row_number() OVER (ORDER BY `+accountOrder(f.Sort)+`)::integer,`+accountSummaryJSON+accountListFrom,
		append(args, snapshot)...)
	if err != nil {
		return page, accountSeek{}, err
	}
	page.Items, err = readAccountSnapshotItems(ctx, tx, accountSeek{Snapshot: snapshot}, f.Limit)
	if err != nil {
		return page, accountSeek{}, err
	}
	return page, accountSeek{Snapshot: snapshot, Ordinal: f.Limit}, tx.Commit(ctx)
}

func saveAccountSnapshot(ctx context.Context, tx pgx.Tx, id uuid.UUID, actor managerActor,
	page accountPage, queryHash [32]byte) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM platform_manager.account_snapshots WHERE expires_at<=transaction_timestamp()
		  OR id IN (
			SELECT id FROM platform_manager.account_snapshots WHERE subject_id=$1
			ORDER BY expires_at DESC,id DESC OFFSET $2
		  )`, actor.Subject, accountEmployeeSnapshots-1); err != nil {
		return err
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_manager.account_snapshots`).Scan(&total); err != nil {
		return err
	}
	if total >= accountGlobalSnapshots {
		return errManagerSnapshotLimit
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.account_snapshots(id,subject_id,grants_version,query_hash,scope_hash,snapshot_at,expires_at,total)
		VALUES($1,$2,$3,$4,`+approvedScopeHash+`,$5,$6,$7)`, id, actor.Subject, actor.GrantsVersion,
		queryHash[:], page.SnapshotAt, page.SnapshotAt.Add(managerSnapshotTTL), page.Summary.Total)
	return err
}

const accountListFrom = `
	FROM platform_manager.visible_accounts a ` + accountObservationJoins + `
	WHERE ($1='' OR strpos(a.amo_account_id::text,$1)>0 OR strpos(lower(COALESCE(p.display_name,i.display_name)),lower($1))>0
		OR strpos(lower(i.current_domain),lower($1))>0
		OR EXISTS (SELECT FROM platform_manager.account_domains d WHERE d.account_id=a.account_id
			AND d.domain<>i.current_domain AND strpos(lower(d.domain),lower($1))>0))
	  AND (COALESCE(cardinality($2::text[]),0)=0 OR p.classification=ANY($2))
	  AND ($3::uuid IS NULL OR EXISTS (
		SELECT FROM platform_manager.account_memberships p WHERE p.account_id=a.account_id AND p.product_id=$3))
	  AND $4::timestamptz IS NULL
	  AND ($5::integer IS NULL OR u.active_count>=$5)
	  AND ($6::integer IS NULL OR u.active_count<=$6)`

func continueAccounts(ctx context.Context, tx pgx.Tx, actor managerActor, f accountFilters, after accountSeek,
	page accountPage, queryHash [32]byte) (accountPage, accountSeek, error) {
	err := tx.QueryRow(ctx, `
		SELECT snapshot_at,total FROM platform_manager.account_snapshots
		WHERE id=$1 AND subject_id=$2 AND grants_version=$3 AND query_hash=$4
		  AND expires_at>statement_timestamp() AND scope_hash=`+approvedScopeHash,
		after.Snapshot, actor.Subject, actor.GrantsVersion, queryHash[:]).Scan(&page.SnapshotAt, &page.Summary.Total)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, accountSeek{}, errManagerCursor
	}
	if err != nil {
		return page, accountSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	page.Items, err = readAccountSnapshotItems(ctx, tx, after, f.Limit)
	if err != nil {
		return page, accountSeek{}, err
	}
	if len(page.Items) == 0 {
		return page, accountSeek{}, errManagerCursor
	}
	after.Ordinal += len(page.Items)
	if after.Ordinal == page.Summary.Total {
		after = accountSeek{}
	}
	return page, after, tx.Commit(ctx)
}

func readAccountSnapshotItems(ctx context.Context, tx pgx.Tx, after accountSeek, limit int) ([]json.RawMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT item FROM platform_manager.account_snapshot_items
		WHERE snapshot_id=$1 AND ordinal>$2 ORDER BY ordinal LIMIT $3`, after.Snapshot, after.Ordinal, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
}

func readAccountDetail(ctx context.Context, db accountScopeReader, id uuid.UUID, actions []accountCapability) (json.RawMessage, int64, error) {
	var body json.RawMessage
	var version int64
	err := db.QueryRow(ctx, `
		SELECT `+accountSummaryJSON+` || jsonb_build_object(
			'note',p.note,'region',NULL,'time_zone',i.time_zone,'amostart',`+unsupportedAccountObservation+`,'capabilities',$2::jsonb),
			COALESCE(p.version,1)
		FROM platform_manager.visible_accounts a `+accountObservationJoins+` WHERE a.account_id=$1`, id, actions).Scan(&body, &version)
	return body, version, accountReadError(err)
}

func (s managerStore) accountCRMUsers(ctx context.Context, id uuid.UUID, active, admin *bool) (json.RawMessage, error) {
	var body json.RawMessage
	err := s.pool.QueryRow(ctx, `
		SELECT jsonb_strip_nulls(jsonb_build_object(`+rosterObservationFields+`,
			'value',CASE WHEN u.roster IS NOT NULL THEN
				COALESCE((SELECT jsonb_agg(item ORDER BY ordinal)
					FROM jsonb_array_elements(u.roster) WITH ORDINALITY AS filtered(item,ordinal)
					WHERE ($2::boolean IS NULL OR (item->>'is_active')::boolean=$2)
					  AND ($3::boolean IS NULL OR (item->>'is_admin')::boolean=$3)), '[]'::jsonb) END))
		FROM platform_manager.visible_accounts a LEFT JOIN platform_manager.account_rosters u USING(account_id)
		WHERE a.account_id=$1`, id, active, admin).Scan(&body)
	return body, accountReadError(err)
}

func (s managerStore) accountVisibleAt(ctx context.Context, id uuid.UUID) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT transaction_timestamp() FROM platform_manager.visible_accounts WHERE account_id=$1`, id).Scan(&at)
	return at.UTC(), accountReadError(err)
}

func accountReadError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errManagerNotFound
	}
	return err
}
