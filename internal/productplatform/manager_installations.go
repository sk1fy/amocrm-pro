package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	managerSnapshotTTL       = 5 * time.Minute
	managerSnapshotRows      = 10000
	managerEmployeeSnapshots = 4
	managerGlobalSnapshots   = 32
)

var (
	errManagerNotFound      = errors.New("manager resource not found")
	errManagerConflict      = errors.New("manager command identity conflict")
	errManagerCursor        = errors.New("invalid manager cursor")
	errManagerSnapshotLimit = errors.New("manager snapshot admission limit")
	errManagerQueryTooBroad = errors.New("manager query exceeds snapshot capacity")
)

type managerStore struct {
	pool *pgxpool.Pool
}

type installationReview struct {
	Reviewed bool       `json:"reviewed"`
	By       *uuid.UUID `json:"reviewed_by,omitempty"`
	At       *time.Time `json:"reviewed_at,omitempty"`
}

type managerInstallation struct {
	TrialExtensionDays int                  `json:"trial_extension_days,omitempty"`
	WidgetVersion      *string              `json:"widget_version,omitempty"`
	InstalledBy        *string              `json:"installed_by,omitempty"`
	ID                 uuid.UUID            `json:"id"`
	Account            uuid.UUID            `json:"account_id"`
	AmoAccount         string               `json:"amo_account_id"`
	Product            uuid.UUID            `json:"product_id"`
	ProductCode        string               `json:"product_code"`
	ProductName        string               `json:"product_name"`
	ProductLine        string               `json:"product_line,omitempty"`
	AccountDomain      *string              `json:"account_domain,omitempty"`
	AccountName        *string              `json:"account_name,omitempty"`
	AccountClass       *string              `json:"account_classification,omitempty"`
	AccountZone        *string              `json:"account_time_zone,omitempty"`
	Installation       uuid.UUID            `json:"core_installation_id"`
	Generation         int64                `json:"generation"`
	Kind               string               `json:"kind"`
	InstalledAt        time.Time            `json:"installed_at"`
	RemovedAt          *time.Time           `json:"removed_at"`
	LastLifecycle      string               `json:"last_lifecycle_event"`
	Lifecycle          string               `json:"lifecycle"`
	Authorization      string               `json:"authorization"`
	Activation         string               `json:"activation_status"`
	Review             installationReview   `json:"review"`
	CRMWorkflow        string               `json:"crm_workflow"`
	Access             accessState          `json:"access"`
	Contact            *installationContact `json:"contact,omitempty"`
	Deal               *installationDeal    `json:"deal,omitempty"`
	Version            int64                `json:"version"`
}

type installationFilters struct {
	Q, Review, Kind, LastLifecycle, Line, Activation, Access string
	Product                                                  *uuid.UUID
	Account                                                  *uuid.UUID `json:"Account,omitempty"`
	Lifecycle                                                string     `json:"Lifecycle,omitempty"`
	From, To, Zone                                           string
	start, before                                            *time.Time
	Limit                                                    int
	Ascending                                                bool
	requireKnownTariff                                       bool
}

func (f installationFilters) hash() [32]byte {
	body, _ := json.Marshal(f)
	return sha256.Sum256(body)
}

type installationSummary struct {
	Total      int `json:"total"`
	Reviewed   int `json:"reviewed"`
	Unreviewed int `json:"unreviewed"`
}

type installationPage struct {
	Items       []managerInstallation `json:"items"`
	Next        *string               `json:"next_cursor"`
	SnapshotAt  time.Time             `json:"snapshot_at"`
	QueryHash   string                `json:"query_hash"`
	Summary     installationSummary   `json:"summary"`
	QueueCounts *installationSummary  `json:"queue_counts,omitempty"`
}

type installationSeek struct {
	Snapshot uuid.UUID
	At       time.Time
	ID       uuid.UUID
}

const installationColumns = `inst.id,inst.account_id,amo_account_id,product_id,product_code,product_name,COALESCE(line,''),
	inst.installation_id,inst.generation,inst.kind,opened_at,closed_at,last_lifecycle_event,lifecycle,
	"authorization",reviewed_by,reviewed_at,valid_until,term_source,entitlement_version,inst.version, CASE WHEN lifecycle='current' AND "authorization"='authorized' THEN package_until END,
	i.current_domain,COALESCE(p.display_name,i.display_name),p.classification,i.time_zone,
	w.amo_user_id,w.name,w.phone,w.email,w.role,
	d.status,d.deal_id,d.deal_host,d.reason,d.retry_version,j.status,j.available_at,d.title IS NULL,t.numeric_value,t.observed_at,
	(SELECT ep.installed_by::text FROM platform_lifecycle.episodes ep WHERE ep.id=inst.id),versions.widget_version,(SELECT trial_extension_days FROM platform_lifecycle.products WHERE id=inst.product_id)`

const installationFrom = ` FROM platform_manager.installation_directory inst ` + installationProfileJoins

const installationProfileJoins = `
	LEFT JOIN platform_manager.installation_widget_versions versions USING(installation_id,generation)
	LEFT JOIN platform_manager.subscription_terms USING(account_id,product_id)
	LEFT JOIN platform_manager.account_identities i USING(account_id)
	LEFT JOIN platform_manager.account_profiles p USING(account_id)
	LEFT JOIN platform_manager.installation_contacts w ON w.episode_id=inst.id
	LEFT JOIN platform_manager.installation_deals d ON d.episode_id=inst.id
	LEFT JOIN platform_jobs.jobs j ON j.id=d.episode_id
	LEFT JOIN platform_manager.account_tariff_fields t ON t.account_id=inst.account_id AND t.field='tariff_id'`

const installationListWhere = `
	WHERE ($1='' OR strpos(amo_account_id::text, $1)>0 OR strpos(lower(product_name), lower($1))>0
		OR strpos(lower(product_code), lower($1))>0 OR strpos(lower(i.current_domain), lower($1))>0
		OR strpos(lower(COALESCE(p.display_name,i.display_name)), lower($1))>0
		OR strpos(lower(w.name),lower($1))>0 OR strpos(lower(w.email),lower($1))>0
		OR ($1 ~ '^[0-9()+ .-]+$' AND length(regexp_replace($1,'[^0-9]','','g'))>=3
			AND strpos(w.phone,regexp_replace(regexp_replace($1,'[^0-9]','','g'),'^8([0-9]{10})$','7\1'))>0))
	  AND ($2='' OR inst.kind=$2)
	  AND ($3='' OR last_lifecycle_event=$3)
	  AND ($4::uuid IS NULL OR product_id=$4)
	  AND ($5='' OR line=$5)
	  AND ($6='' OR $6=CASE WHEN w.episode_id IS NOT NULL THEN 'activated' ELSE 'unavailable' END)
	  AND ($7='' OR (COALESCE(valid_until>$10 AND term_source IN ('own_trial','free','own_paid','manual'), false)
        OR COALESCE(lifecycle='current' AND "authorization"='authorized' AND package_until>$10,false)) = ($7='active'))
	  AND ($8::timestamptz IS NULL OR opened_at >= $8)
	  AND ($9::timestamptz IS NULL OR opened_at < $9)
	  AND ($11::uuid IS NULL OR inst.account_id=$11)
	  AND ($12='' OR lifecycle=$12)`

func scanManagerInstallation(row pgx.Row, now time.Time, requireKnownTariff bool) (managerInstallation, error) {
	var item managerInstallation
	var account int64
	var access accessProjection
	var userID, dealID *int64
	var name, phone, email, role, dealState, dealHost, dealReason, jobState *string
	var dealVersion *int
	var nextRun, tariffObservedAt *time.Time
	var tariffID *int64
	var unreserved bool
	err := row.Scan(&item.ID, &item.Account, &account, &item.Product, &item.ProductCode, &item.ProductName, &item.ProductLine,
		&item.Installation, &item.Generation, &item.Kind, &item.InstalledAt, &item.RemovedAt, &item.LastLifecycle,
		&item.Lifecycle, &item.Authorization, &item.Review.By, &item.Review.At, &access.until, &access.source,
		&access.version, &item.Version, &access.packageUntil, &item.AccountDomain, &item.AccountName, &item.AccountClass, &item.AccountZone,
		&userID, &name, &phone, &email, &role, &dealState, &dealID, &dealHost, &dealReason, &dealVersion, &jobState, &nextRun,
		&unreserved, &tariffID, &tariffObservedAt, &item.InstalledBy, &item.WidgetVersion, &item.TrialExtensionDays)
	if err != nil {
		return item, err
	}
	item.AmoAccount = strconv.FormatInt(account, 10)
	item.InstalledAt = item.InstalledAt.UTC()
	item.RemovedAt, item.Review.At = managerUTC(item.RemovedAt), managerUTC(item.Review.At)
	item.Review.Reviewed = item.Review.By != nil
	item.Activation, item.CRMWorkflow = "unavailable", "unsupported"
	if userID != nil {
		item.Contact = &installationContact{UserID: strconv.FormatInt(*userID, 10), Name: *name, Phone: *phone, Email: email, Role: role}
		item.Activation = "activated"
	}
	if dealState != nil {
		item.Deal = &installationDeal{State: *dealState, URL: dealURL(dealID, dealHost), Reason: dealReason, RetryVersion: *dealVersion}
		if dealID != nil {
			item.Deal.ID = new(strconv.FormatInt(*dealID, 10))
		}
		if jobState != nil && *jobState == "queued" {
			item.Deal.NextRun = managerUTC(nextRun)
		}
		parked := dealReason != nil && *dealReason == dealTariffUnknown
		if parked {
			item.Deal.Reason = nil
		}
		waitingForTariff := requireKnownTariff && unreserved && (tariffID == nil || *tariffID != technicalAccountTariffID) &&
			(tariffObservedAt == nil || tariffObservedAt.Before(item.InstalledAt)) && jobState != nil && (*jobState == "queued" || *jobState == "running")
		if *dealState == "pending" && item.Kind == "install" && item.Lifecycle == "current" &&
			(item.AccountClass == nil || *item.AccountClass != "Запрет работы") && now.Sub(item.InstalledAt) >= 24*time.Hour &&
			(parked || waitingForTariff) {
			item.Deal.Reason = new(dealTariffUnknown)
			item.Deal.NextRun = nil
		}
		if jobState != nil && *jobState == "dead" && *dealState != "skipped" && *dealState != "succeeded" {
			item.Deal.CanRetry = true
			if *dealState == "pending" {
				item.Deal.State = "failed"
			}
		}
		switch item.Deal.State {
		case "reconciling":
			item.CRMWorkflow = "needs_review"
		case "pending", "succeeded", "failed":
			item.CRMWorkflow = item.Deal.State
		}
	}
	access.account, access.product = item.Account, item.Product
	item.Access = access.state(now)
	return item, nil
}

func managerUTC(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

const approvedScopeHash = `(SELECT sha256(convert_to(COALESCE(string_agg(
	integration_id::text || '/' || product_id::text || '/' || amo_account_id::text, ','
	ORDER BY integration_id, product_id, amo_account_id), ''), 'UTF8'))
	FROM platform_manager.approved_pairs)`

func (s managerStore) list(ctx context.Context, actor managerActor, filters installationFilters, after installationSeek) (installationPage, installationSeek, error) {
	queryHash := filters.hash()
	page := installationPage{
		Items: make([]managerInstallation, 0, filters.Limit), QueryHash: hex.EncodeToString(queryHash[:]),
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, installationSeek{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if after.Snapshot != uuid.Nil {
		return continueInstallations(ctx, tx, actor, filters, after, page, queryHash)
	}
	if filters.Account != nil {
		var visible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_manager.visible_accounts WHERE account_id=$1)`, filters.Account).Scan(&visible); err != nil {
			return page, installationSeek{}, err
		}
		if !visible {
			return page, installationSeek{}, errManagerNotFound
		}
	}
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&page.SnapshotAt); err != nil {
		return page, installationSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	args := []any{filters.Q, filters.Kind, filters.LastLifecycle, filters.Product, filters.Line, filters.Activation,
		filters.Access, filters.start, filters.before, page.SnapshotAt, filters.Account, filters.Lifecycle}
	rows, err := tx.Query(ctx, `SELECT `+installationColumns+installationFrom+installationListWhere+`
		AND ($13='' OR (inst.kind<>'observed' AND (reviewed_by IS NOT NULL) = ($13='reviewed')))
		ORDER BY `+installationOrder(filters.Ascending, "opened_at", "inst.id")+` LIMIT $14`,
		append(args, filters.Review, managerSnapshotRows+1)...)
	if err != nil {
		return page, installationSeek{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (managerInstallation, error) {
		return scanManagerInstallation(row, page.SnapshotAt, filters.requireKnownTariff)
	})
	if err != nil {
		return page, installationSeek{}, err
	}
	if len(items) > managerSnapshotRows {
		return page, installationSeek{}, errManagerQueryTooBroad
	}
	page.Summary.Total = len(items)
	for _, item := range items {
		if item.Review.Reviewed {
			page.Summary.Reviewed++
		}
	}
	page.Summary.Unreviewed = page.Summary.Total - page.Summary.Reviewed
	counts := page.Summary
	if filters.Review != "" {
		var opposite int
		if err := tx.QueryRow(ctx, `SELECT count(*)`+installationFrom+installationListWhere+
			` AND inst.kind<>'observed' AND (reviewed_by IS NOT NULL) <> ($13='reviewed')`, append(args, filters.Review)...).Scan(&opposite); err != nil {
			return page, installationSeek{}, err
		}
		counts.Total += opposite
		if filters.Review == "reviewed" {
			counts.Unreviewed = opposite
		} else {
			counts.Reviewed = opposite
		}
	}
	page.QueueCounts = &counts
	page.Items = append(page.Items, items[:min(len(items), filters.Limit)]...)
	if len(items) <= filters.Limit {
		return page, installationSeek{}, tx.Commit(ctx)
	}
	var scopeHash []byte
	if err := tx.QueryRow(ctx, `SELECT `+approvedScopeHash).Scan(&scopeHash); err != nil {
		return page, installationSeek{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return page, installationSeek{}, err
	}

	snapshot := uuid.New()
	if err := s.saveInstallationSnapshot(ctx, snapshot, actor, queryHash, scopeHash, page, items); err != nil {
		return page, installationSeek{}, err
	}
	return page, nextInstallationSeek(snapshot, page.Items), nil
}

func installationOrder(ascending bool, timestamp, id string) string {
	if ascending {
		return timestamp + " ASC," + id + " ASC"
	}
	return timestamp + " DESC," + id + " DESC"
}

func nextInstallationSeek(snapshot uuid.UUID, page []managerInstallation) installationSeek {
	last := page[len(page)-1]
	return installationSeek{Snapshot: snapshot, At: last.InstalledAt, ID: last.ID}
}

type installationSnapshotItem struct {
	managerInstallation
	QueueCounts *installationSummary `json:"queue_counts,omitempty"`
}

func continueInstallations(ctx context.Context, tx pgx.Tx, actor managerActor, filters installationFilters,
	after installationSeek, page installationPage, queryHash [32]byte) (installationPage, installationSeek, error) {
	err := tx.QueryRow(ctx, `
		SELECT snapshot_at,total,reviewed FROM platform_manager.snapshots
		WHERE id=$1 AND subject_id=$2 AND grants_version=$3 AND query_hash=$4
		  AND expires_at>statement_timestamp() AND scope_hash=`+approvedScopeHash,
		after.Snapshot, actor.Subject, actor.GrantsVersion, queryHash[:]).
		Scan(&page.SnapshotAt, &page.Summary.Total, &page.Summary.Reviewed)
	if errors.Is(err, pgx.ErrNoRows) {
		return page, installationSeek{}, errManagerCursor
	}
	if err != nil {
		return page, installationSeek{}, err
	}
	page.SnapshotAt = page.SnapshotAt.UTC()
	page.Summary.Unreviewed = page.Summary.Total - page.Summary.Reviewed
	comparison := "<"
	if filters.Ascending {
		comparison = ">"
	}
	rows, err := tx.Query(ctx, `
		SELECT item FROM platform_manager.snapshot_items
		WHERE snapshot_id=$1 AND (installed_at,episode_id)`+comparison+`($2,$3)
		ORDER BY `+installationOrder(filters.Ascending, "installed_at", "episode_id")+` LIMIT $4`,
		after.Snapshot, after.At, after.ID, filters.Limit+1)
	if err != nil {
		return page, installationSeek{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowTo[installationSnapshotItem])
	if err != nil {
		return page, installationSeek{}, err
	}
	if len(items) == 0 {
		return page, installationSeek{}, errManagerCursor
	}
	page.QueueCounts = items[0].QueueCounts
	for _, item := range items[:min(len(items), filters.Limit)] {
		page.Items = append(page.Items, item.managerInstallation)
	}
	if err := tx.Commit(ctx); err != nil {
		return page, installationSeek{}, err
	}
	if len(items) > filters.Limit {
		return page, nextInstallationSeek(after.Snapshot, page.Items), nil
	}
	return page, installationSeek{}, nil
}

func (s managerStore) saveInstallationSnapshot(ctx context.Context, id uuid.UUID, actor managerActor, queryHash [32]byte,
	scopeHash []byte, page installationPage, items []managerInstallation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One database lock keeps both snapshot limits exact across Platform replicas.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1579107215, 1)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM platform_manager.snapshots WHERE id IN (
			SELECT id FROM platform_manager.snapshots WHERE expires_at<=statement_timestamp()
			ORDER BY expires_at,id LIMIT 4
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM platform_manager.snapshots WHERE id IN (
			SELECT id FROM platform_manager.snapshots WHERE subject_id=$1
			ORDER BY expires_at DESC,id DESC OFFSET $2
		)`, actor.Subject, managerEmployeeSnapshots-1); err != nil {
		return err
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_manager.snapshots`).Scan(&total); err != nil {
		return err
	}
	if total >= managerGlobalSnapshots {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return errManagerSnapshotLimit
	}

	if _, err := tx.Exec(ctx, `INSERT INTO platform_manager.snapshots
		(id,subject_id,grants_version,query_hash,scope_hash,snapshot_at,expires_at,total,reviewed)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, actor.Subject, actor.GrantsVersion, queryHash[:], scopeHash,
		page.SnapshotAt, page.SnapshotAt.Add(managerSnapshotTTL), page.Summary.Total, page.Summary.Reviewed); err != nil {
		return err
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"platform_manager", "snapshot_items"},
		[]string{"snapshot_id", "installed_at", "episode_id", "item"},
		pgx.CopyFromSlice(len(items), func(i int) ([]any, error) {
			item := items[i]
			return []any{id, item.InstalledAt, item.ID, installationSnapshotItem{item, page.QueueCounts}}, nil
		}))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
