package productplatform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type productOverview struct {
	Summary     productOverviewSummary     `json:"summary"`
	Statistics  map[uuid.UUID]productStats `json:"statistics"`
	Connections []productConnectionProblem `json:"connections"`
}

type productOverviewSummary struct {
	Total              int            `json:"total"`
	ByLine             map[string]int `json:"by_line"`
	Installations      int            `json:"installations"`
	Current            int            `json:"current"`
	Recent             int            `json:"recent"`
	ActivationEligible int            `json:"activation_eligible"`
	Activated          int            `json:"activated"`
	Attention          int            `json:"attention"`
	ConnectionCount    int            `json:"connection_count"`
}

type productMonth struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

type productStats struct {
	Months             []productMonth `json:"months"`
	Installations      int            `json:"installations"`
	Current            int            `json:"current"`
	Retained           int            `json:"retained"`
	Recent             int            `json:"recent"`
	ActivationEligible int            `json:"activation_eligible"`
	Activated          int            `json:"activated"`
	ConnectionCount    int            `json:"connection_count"`
	MissingInstallLink bool           `json:"missing_install_link"`
	MissingInstruction bool           `json:"missing_instruction"`
}

func (s productStats) needsAttention() bool {
	return s.ConnectionCount > 0 || s.MissingInstallLink || s.MissingInstruction ||
		s.ActivationEligible > 0 && s.Activated*100 < s.ActivationEligible*90
}

type productConnectionProblem struct {
	InstallationID uuid.UUID  `json:"installation_id"`
	ProductID      uuid.UUID  `json:"product_id"`
	Name           string     `json:"name"`
	Line           string     `json:"line"`
	AccountID      uuid.UUID  `json:"account_id"`
	AmoAccountID   string     `json:"amo_account_id"`
	AccountName    *string    `json:"account_name"`
	AccountDomain  *string    `json:"account_domain"`
	Since          *time.Time `json:"since"`
}

type catalogStatsRow struct {
	ID      uuid.UUID
	Code    string
	Name    string
	Line    string
	Matches bool
	Stats   productStats
}

const productCatalogStatsSQL = `
	WITH native AS (
		SELECT e.id,e.product_id,pair.integration_id,e.opened_at,e.closed_at,c.activated_at
		FROM platform_manager.installations e
		JOIN platform_lifecycle.account_products pair USING(installation_id)
		LEFT JOIN platform_manager.installation_contacts c ON c.episode_id=e.id AND c.activated_at<=$1
		WHERE e.opened_at<=$1
	),
	activation_coverage AS (
		SELECT product_id,integration_id,min(opened_at) AS covered_from
		FROM native WHERE activated_at IS NOT NULL GROUP BY product_id,integration_id
	),
	annual AS (
		SELECT n.product_id,count(*) AS installations,
			count(*) FILTER (WHERE n.closed_at IS NULL) AS retained,
			count(*) FILTER (WHERE n.opened_at>=$4) AS recent,
			count(*) FILTER (WHERE n.opened_at>=coverage.covered_from) AS eligible,
			count(n.activated_at) AS activated
		FROM native n LEFT JOIN activation_coverage coverage USING(product_id,integration_id)
		WHERE n.opened_at>=$2 GROUP BY n.product_id
	),
	monthly AS (
		SELECT product_id,to_char(opened_at AT TIME ZONE $3,'YYYY-MM') AS month,count(*) AS n
		FROM native WHERE opened_at>=$2 GROUP BY product_id,month
	),
	months AS (
		SELECT product_id,jsonb_object_agg(month,n) AS counts FROM monthly GROUP BY product_id
	),
	current_products AS (
		SELECT product_id,count(*) AS total FROM platform_manager.account_product_directory
		WHERE current GROUP BY product_id
	),
	connection_problems AS (
		SELECT product_id,count(*) AS total FROM platform_manager.account_memberships
		WHERE current AND effective_authorization='reauth_required' GROUP BY product_id
	)
	SELECT p.id,p.code,p.display_name,p.line,
		($5='' OR strpos(lower(p.code),lower($5))>0 OR strpos(lower(p.display_name),lower($5))>0)
		AND ($6='' OR p.line=$6) AND ($7='' OR p.availability=$7),
		COALESCE(months.counts,'{}'::jsonb),
		COALESCE(a.installations,0),COALESCE(c.total,0),COALESCE(a.retained,0),COALESCE(a.recent,0),
		COALESCE(a.eligible,0),COALESCE(a.activated,0),COALESCE(cp.total,0),
		p.availability IN ('pilot','active') AND NOT EXISTS (
			SELECT FROM jsonb_array_elements(p.links) link WHERE lower(btrim(link->>'label')) IN ('установка','установить','ссылка на установку')
		),
		p.availability IN ('pilot','active') AND NOT EXISTS (
			SELECT FROM jsonb_array_elements(p.links) link WHERE lower(btrim(link->>'label')) IN ('инструкция','документация')
		)
	FROM platform_lifecycle.products p
	LEFT JOIN annual a ON a.product_id=p.id
	LEFT JOIN months ON months.product_id=p.id
	LEFT JOIN current_products c ON c.product_id=p.id
	LEFT JOIN connection_problems cp ON cp.product_id=p.id
	WHERE p.catalog_version IS NOT NULL ORDER BY p.code,p.id`

func readCatalogStats(ctx context.Context, tx pgx.Tx, f productFilters, at time.Time, zone *time.Location) ([]catalogStatsRow, error) {
	local := at.In(zone)
	first := time.Date(local.Year(), local.Month()-11, 1, 0, 0, 0, 0, zone)
	rows, err := tx.Query(ctx, productCatalogStatsSQL, at, first, zone.String(), at.Add(-30*24*time.Hour),
		f.Q, f.Line, f.Availability)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (catalogStatsRow, error) {
		var r catalogStatsRow
		var monthly map[string]int
		s := &r.Stats
		err := row.Scan(&r.ID, &r.Code, &r.Name, &r.Line, &r.Matches, &monthly, &s.Installations, &s.Current, &s.Retained,
			&s.Recent, &s.ActivationEligible, &s.Activated, &s.ConnectionCount, &s.MissingInstallLink, &s.MissingInstruction)
		s.Months = make([]productMonth, 12)
		for i := range s.Months {
			month := first.AddDate(0, i, 0).Format("2006-01")
			s.Months[i] = productMonth{Month: month, Count: monthly[month]}
		}
		return r, err
	})
}

func readConnectionProblems(ctx context.Context, tx pgx.Tx) ([]productConnectionProblem, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.installation_id,p.id,p.display_name,p.line,m.account_id,m.amo_account_id::text,
			COALESCE(profile.display_name,i.display_name),i.current_domain,h.occurred_at
		FROM platform_manager.account_memberships m
		JOIN platform_lifecycle.products p ON p.id=m.product_id AND p.catalog_version IS NOT NULL
		LEFT JOIN platform_manager.account_identities i USING(account_id)
		LEFT JOIN platform_manager.account_profiles profile USING(account_id)
		LEFT JOIN platform_lifecycle.inbox event ON event.installation_id=m.installation_id AND event.sequence=m.authorization_sequence
		LEFT JOIN platform_lifecycle.history h ON h.event_id=event.event_id AND h.kind='authorization_changed'
		WHERE m.current AND m.effective_authorization='reauth_required'
		ORDER BY h.occurred_at DESC NULLS LAST,m.installation_id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (productConnectionProblem, error) {
		var problem productConnectionProblem
		err := row.Scan(&problem.InstallationID, &problem.ProductID, &problem.Name, &problem.Line, &problem.AccountID,
			&problem.AmoAccountID, &problem.AccountName, &problem.AccountDomain, &problem.Since)
		return problem, err
	})
}

func readProductOverview(ctx context.Context, tx pgx.Tx, f productFilters, after productSeek, page productPage, revision int64, zone *time.Location) (productPage, productSeek, error) {
	rows, err := readCatalogStats(ctx, tx, f, page.SnapshotAt, zone)
	if err != nil {
		return page, productSeek{}, err
	}
	overview := &productOverview{
		Summary:     productOverviewSummary{ByLine: map[string]int{"FREE": 0, "TOP": 0, "PRO": 0}},
		Statistics:  make(map[uuid.UUID]productStats),
		Connections: []productConnectionProblem{},
	}
	summary := &overview.Summary
	matching := make([]catalogStatsRow, 0, len(rows))
	for _, row := range rows {
		s := row.Stats
		summary.Total++
		summary.ByLine[row.Line]++
		summary.Installations += s.Installations
		summary.Current += s.Current
		summary.Recent += s.Recent
		summary.ActivationEligible += s.ActivationEligible
		summary.Activated += s.Activated
		summary.ConnectionCount += s.ConnectionCount
		if s.needsAttention() {
			summary.Attention++
		}
		if row.Matches && (!f.Attention || s.needsAttention()) {
			matching = append(matching, row)
		}
	}
	if summary.ConnectionCount > 0 {
		overview.Connections, err = readConnectionProblems(ctx, tx)
		if err != nil {
			return page, productSeek{}, err
		}
	}
	encoded, err := json.Marshal(struct {
		Rows        []catalogStatsRow
		Connections []productConnectionProblem
	}{rows, overview.Connections})
	if err != nil {
		return page, productSeek{}, err
	}
	digest := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(digest[:])
	if after.ID != uuid.Nil && after.Overview != fingerprint {
		return page, productSeek{}, errManagerCursor
	}

	switch f.Sort {
	case "name":
		slices.SortStableFunc(matching, func(a, b catalogStatsRow) int {
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
	case "code":
	default:
		slices.SortStableFunc(matching, func(a, b catalogStatsRow) int {
			return b.Stats.Installations - a.Stats.Installations
		})
	}
	if after.ID != uuid.Nil {
		i := slices.IndexFunc(matching, func(row catalogStatsRow) bool { return row.ID == after.ID })
		if i == -1 {
			return page, productSeek{}, errManagerCursor
		}
		matching = matching[i+1:]
	}
	page.Overview = overview
	page.Items = []managerProduct{}
	batch := matching[:min(len(matching), f.Limit)]
	if len(batch) > 0 {
		ids := make([]uuid.UUID, 0, len(batch))
		for _, item := range batch {
			ids = append(ids, item.ID)
			overview.Statistics[item.ID] = item.Stats
		}
		products, err := tx.Query(ctx, `SELECT `+productListColumns+productFrom+`
			WHERE p.id=ANY($1::uuid[]) ORDER BY array_position($1::uuid[],p.id)`, ids)
		if err != nil {
			return page, productSeek{}, err
		}
		page.Items, err = pgx.CollectRows(products, func(row pgx.CollectableRow) (managerProduct, error) { return scanProduct(row) })
		if err != nil {
			return page, productSeek{}, err
		}
	}
	var next productSeek
	if len(matching) > len(batch) {
		last := batch[len(batch)-1]
		next = productSeek{Revision: revision, Code: last.Code, ID: last.ID, SnapshotAt: page.SnapshotAt, Overview: fingerprint}
	}
	return page, next, tx.Commit(ctx)
}
