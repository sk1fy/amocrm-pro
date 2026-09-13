package adminread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type store struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

const recentFailedJobsSQL = `(SELECT count(*) FROM jobs j
 WHERE j.installation_id = i.id AND j.status IN ('failed', 'dead')
   AND j.updated_at > now() - interval '24 hours')`

func (s *store) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

type installationListFilter struct {
	AccountID     *int64
	Domain        string
	IntegrationID *uuid.UUID
	Status        string
	WebhookStatus string
	Query         accountQuery
	Limit         int
	CursorTime    *time.Time
	CursorID      string
}

type jobListFilter struct {
	InstallationID *uuid.UUID
	Status         string
	Type           string
	Since          *time.Time
	Limit          int
	CursorTime     *time.Time
	CursorID       string
}

type auditListFilter struct {
	InstallationID *uuid.UUID
	ObjectType     string
	ObjectID       string
	Action         string
	Limit          int
	CursorTime     *time.Time
	CursorID       string
}

type installationRow struct {
	summary       InstallationSummary
	facts         AuthorizationFacts
	webhook       WebhookInfo
	pilotEnabled  *bool
	integrationID uuid.UUID
	isFixture     bool
}

func (s *store) listAccounts(ctx context.Context, f installationListFilter) ([]AccountListItem, *string, *int64, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	now := time.Now().UTC()

	totalFilter := f
	totalFilter.CursorTime = nil
	totalFilter.CursorID = ""
	var totalB strings.Builder
	totalArgs := make([]any, 0, 12)
	totalB.WriteString(`SELECT count(DISTINCT i.account_id)
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
WHERE 1=1`)
	appendInstallationFilters(&totalB, &totalArgs, totalFilter)
	var total int64
	if err := s.pool.QueryRow(ctx, totalB.String(), totalArgs...).Scan(&total); err != nil {
		return nil, nil, nil, fmt.Errorf("count accounts: %w", err)
	}

	var b strings.Builder
	args := make([]any, 0, 12)
	b.WriteString(`SELECT i.account_id, max(i.updated_at) AS last_activity_at
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
WHERE 1=1`)
	appendInstallationFilters(&b, &args, f)
	if f.CursorTime != nil && f.CursorID != "" {
		accountID, err := strconv.ParseInt(f.CursorID, 10, 64)
		if err != nil {
			return nil, nil, nil, errInvalid("invalid cursor")
		}
		fmt.Fprintf(&b, " GROUP BY i.account_id HAVING (max(i.updated_at), i.account_id) < ($%d::timestamptz, $%d::bigint)", len(args)+1, len(args)+2)
		args = append(args, *f.CursorTime, accountID)
	} else {
		b.WriteString(" GROUP BY i.account_id")
	}
	fmt.Fprintf(&b, " ORDER BY last_activity_at DESC, i.account_id DESC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit+1)

	rows, err := s.pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	type accountKey struct {
		ID             int64
		LastActivityAt time.Time
	}
	keys := make([]accountKey, 0)
	for rows.Next() {
		var key accountKey
		if err := rows.Scan(&key.ID, &key.LastActivityAt); err != nil {
			return nil, nil, nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	rows.Close()
	var next *string
	if len(keys) > f.Limit {
		keys = keys[:f.Limit]
		last := keys[len(keys)-1]
		cursor := encodeCursor(last.LastActivityAt, strconv.FormatInt(last.ID, 10))
		next = &cursor
	}
	if len(keys) == 0 {
		return []AccountListItem{}, nil, &total, nil
	}
	ids := make([]int64, 0, len(keys))
	index := make(map[int64]int, len(keys))
	items := make([]AccountListItem, len(keys))
	for i, key := range keys {
		ids = append(ids, key.ID)
		index[key.ID] = i
		items[i] = AccountListItem{
			AccountID:           key.ID,
			Domains:             make([]string, 0),
			ConnectionsByStatus: make(map[string]int),
			LastActivityAt:      key.LastActivityAt.UTC(),
			Origin:              originReal,
			Installations:       make([]AccountInstallation, 0),
		}
	}

	detail := f
	detail.CursorTime = nil
	detail.CursorID = ""
	var db strings.Builder
	dargs := make([]any, 0, 12+len(ids))
	db.WriteString(`SELECT i.id, i.integration_id, ig.code, i.account_id, i.account_domain, i.status, i.updated_at,
	COALESCE(i.settings->>'origin' = 'fixture', false),
	i.webhook_status, oc.expires_at, oc.lease_until, (oc.installation_id IS NOT NULL),
	` + recentFailedJobsSQL + `
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
LEFT JOIN oauth_credentials oc ON oc.installation_id = i.id
WHERE i.account_id IN (`)
	for i, id := range ids {
		if i > 0 {
			db.WriteString(",")
		}
		fmt.Fprintf(&db, "$%d", i+1)
		dargs = append(dargs, id)
	}
	db.WriteString(")")
	appendInstallationFiltersShifted(&db, &dargs, detail)
	db.WriteString(" ORDER BY i.updated_at DESC, i.id DESC")
	drows, err := s.pool.Query(ctx, db.String(), dargs...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list account installations: %w", err)
	}
	defer drows.Close()
	domainSeen := make(map[int64]map[string]bool, len(keys))
	integrationIDs := make([]uuid.UUID, 0)
	seenIntegrations := make(map[uuid.UUID]bool)
	for drows.Next() {
		var item AccountInstallation
		var accountID int64
		var domain, status, webhookStatus string
		var updated time.Time
		var fixture, credentialsPresent bool
		var expiresAt, leaseUntil *time.Time
		var recentFailedJobs int
		if err := drows.Scan(&item.ID, &item.IntegrationID, &item.IntegrationCode, &accountID, &domain, &status, &updated, &fixture,
			&webhookStatus, &expiresAt, &leaseUntil, &credentialsPresent, &recentFailedJobs); err != nil {
			return nil, nil, nil, err
		}
		pos, ok := index[accountID]
		if !ok {
			continue
		}
		item.Status = status
		item.WebhookStatus = webhookStatus
		item.RecentFailedJobs = recentFailedJobs
		item.Authorization = MapAuthorization(AuthorizationFacts{
			InstallationStatus: status,
			Present:            credentialsPresent,
			ExpiresAt:          expiresAt,
			LeaseUntil:         leaseUntil,
		}, now).State
		acc := &items[pos]
		acc.Installations = append(acc.Installations, item)
		if !seenIntegrations[item.IntegrationID] {
			seenIntegrations[item.IntegrationID] = true
			integrationIDs = append(integrationIDs, item.IntegrationID)
		}
		acc.ConnectionsByStatus[status]++
		if fixture {
			acc.Origin = originFixture
		}
		if domainSeen[accountID] == nil {
			domainSeen[accountID] = make(map[string]bool)
		}
		if !domainSeen[accountID][domain] {
			domainSeen[accountID][domain] = true
			acc.Domains = append(acc.Domains, domain)
		}
	}
	if err := drows.Err(); err != nil {
		return nil, nil, nil, err
	}
	drows.Close()
	grants, err := s.grantsByIntegration(ctx, integrationIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := range items {
		for j := range items[i].Installations {
			installation := &items[i].Installations[j]
			installation.Grants = grants[installation.IntegrationID]
			if installation.Grants == nil {
				installation.Grants = []Grant{}
			}
		}
	}
	return items, next, &total, nil
}

func (s *store) getAccount(ctx context.Context, accountID int64, now time.Time) (AccountResponse, error) {
	cards, err := s.loadInstallationCards(ctx, `i.account_id = $1`, []any{accountID}, now)
	if err != nil {
		return AccountResponse{}, err
	}
	if len(cards) == 0 {
		return AccountResponse{}, errNotFound("account not found")
	}
	domains := make([]string, 0)
	seen := make(map[string]bool)
	origin := originReal
	for _, card := range cards {
		if !seen[card.Installation.AccountDomain] {
			seen[card.Installation.AccountDomain] = true
			domains = append(domains, card.Installation.AccountDomain)
		}
		if card.Installation.Origin == originFixture {
			origin = originFixture
		}
	}
	return AccountResponse{
		AccountID: accountID, Domains: domains, Origin: origin, Installations: cards,
	}, nil
}

func (s *store) listInstallations(ctx context.Context, f installationListFilter) ([]InstallationSummary, *string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var b strings.Builder
	args := make([]any, 0, 12)
	b.WriteString(`SELECT i.id, i.integration_id, ig.code, i.account_id, i.account_domain, i.status, i.installed_by,
	CASE WHEN i.settings->>'origin' = 'fixture' THEN 'fixture' ELSE 'real' END,
	i.created_at, i.updated_at, i.webhook_status,
	` + recentFailedJobsSQL + `
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
WHERE 1=1`)
	appendInstallationFilters(&b, &args, f)
	if f.CursorTime != nil && f.CursorID != "" {
		id, err := uuid.Parse(f.CursorID)
		if err != nil {
			return nil, nil, errInvalid("invalid cursor")
		}
		fmt.Fprintf(&b, " AND (i.updated_at, i.id) < ($%d::timestamptz, $%d::uuid)", len(args)+1, len(args)+2)
		args = append(args, *f.CursorTime, id)
	}
	fmt.Fprintf(&b, " ORDER BY i.updated_at DESC, i.id DESC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]InstallationSummary, 0)
	for rows.Next() {
		var item InstallationSummary
		if err := rows.Scan(
			&item.ID, &item.IntegrationID, &item.IntegrationCode, &item.AccountID, &item.AccountDomain,
			&item.Status, &item.InstalledBy, &item.Origin, &item.CreatedAt, &item.UpdatedAt, &item.WebhookStatus,
			&item.RecentFailedJobs,
		); err != nil {
			return nil, nil, err
		}
		item.CreatedAt = item.CreatedAt.UTC()
		item.UpdatedAt = item.UpdatedAt.UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[len(items)-1]
		cursor := encodeCursor(last.UpdatedAt, last.ID.String())
		next = &cursor
	}
	return items, next, nil
}

func (s *store) getInstallation(ctx context.Context, id uuid.UUID, now time.Time) (InstallationCard, error) {
	cards, err := s.loadInstallationCards(ctx, `i.id = $1`, []any{id}, now)
	if err != nil {
		return InstallationCard{}, err
	}
	if len(cards) == 0 {
		return InstallationCard{}, errNotFound("installation not found")
	}
	return cards[0], nil
}

func (s *store) installationExists(ctx context.Context, id uuid.UUID) (bool, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM installations WHERE id=$1)`, id).Scan(&exists)
	return exists, err
}

func (s *store) loadInstallationCards(ctx context.Context, where string, args []any, now time.Time) ([]InstallationCard, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	query := `SELECT i.id, i.integration_id, ig.code, i.account_id, i.account_domain, i.status, i.installed_by,
	COALESCE(i.settings->>'origin' = 'fixture', false), i.created_at, i.updated_at,
	i.webhook_status, i.webhook_settings, i.webhook_checked_at, i.webhook_last_error,
	oc.expires_at, oc.token_version, oc.key_version, oc.refreshed_at, oc.lease_until,
	(oc.installation_id IS NOT NULL), ap.enabled,
	(SELECT count(*) FROM installation_webhook_destinations d WHERE d.installation_id = i.id),
	` + recentFailedJobsSQL + `
FROM installations i
JOIN integrations ig ON ig.id = i.integration_id
LEFT JOIN oauth_credentials oc ON oc.installation_id = i.id
LEFT JOIN activity_pilots ap ON ap.installation_id = i.id
WHERE ` + where + `
ORDER BY i.updated_at DESC, i.id DESC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rowsOut := make([]installationRow, 0)
	integrationIDs := make([]uuid.UUID, 0)
	seenIntegrations := make(map[uuid.UUID]bool)
	for rows.Next() {
		var row installationRow
		var events []byte
		var tokenVersion *int64
		var keyVersion *int
		if err := rows.Scan(
			&row.summary.ID, &row.integrationID, &row.summary.IntegrationCode, &row.summary.AccountID,
			&row.summary.AccountDomain, &row.summary.Status, &row.summary.InstalledBy, &row.isFixture,
			&row.summary.CreatedAt, &row.summary.UpdatedAt, &row.webhook.Status, &events,
			&row.webhook.CheckedAt, &row.webhook.LastError, &row.facts.ExpiresAt, &tokenVersion,
			&keyVersion, &row.facts.RefreshedAt, &row.facts.LeaseUntil, &row.facts.Present,
			&row.pilotEnabled, &row.webhook.ConfirmedDestinations,
			&row.summary.RecentFailedJobs,
		); err != nil {
			return nil, err
		}
		row.summary.IntegrationID = row.integrationID
		row.summary.Origin = installationOrigin(row.isFixture)
		row.summary.CreatedAt = row.summary.CreatedAt.UTC()
		row.summary.UpdatedAt = row.summary.UpdatedAt.UTC()
		row.summary.WebhookStatus = row.webhook.Status
		row.webhook.Events = decodeStringArray(events)
		if row.webhook.CheckedAt != nil {
			utc := row.webhook.CheckedAt.UTC()
			row.webhook.CheckedAt = &utc
		}
		row.facts.InstallationStatus = row.summary.Status
		if tokenVersion != nil {
			row.facts.TokenVersion = *tokenVersion
		}
		if keyVersion != nil {
			row.facts.KeyVersion = *keyVersion
		}
		if row.facts.ExpiresAt != nil {
			utc := row.facts.ExpiresAt.UTC()
			row.facts.ExpiresAt = &utc
		}
		if row.facts.RefreshedAt != nil {
			utc := row.facts.RefreshedAt.UTC()
			row.facts.RefreshedAt = &utc
		}
		if row.facts.LeaseUntil != nil {
			utc := row.facts.LeaseUntil.UTC()
			row.facts.LeaseUntil = &utc
		}
		rowsOut = append(rowsOut, row)
		if !seenIntegrations[row.integrationID] {
			seenIntegrations[row.integrationID] = true
			integrationIDs = append(integrationIDs, row.integrationID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	grants, err := s.grantsByIntegration(ctx, integrationIDs)
	if err != nil {
		return nil, err
	}
	cards := make([]InstallationCard, 0, len(rowsOut))
	for _, row := range rowsOut {
		g := grants[row.integrationID]
		if g == nil {
			g = []Grant{}
		}
		cards = append(cards, InstallationCard{
			Installation:  row.summary,
			Authorization: MapAuthorization(row.facts, now),
			Webhook:       row.webhook,
			Grants:        g,
			Activity:      ActivityInfo{Pilot: pilotState(row.pilotEnabled)},
		})
	}
	return cards, nil
}

func (s *store) grantsByIntegration(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]Grant, error) {
	out := make(map[uuid.UUID][]Grant, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT integration_id, service_code, enabled
FROM integration_services
WHERE integration_id = ANY($1)
ORDER BY service_code`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var grant Grant
		if err := rows.Scan(&id, &grant.Service, &grant.Enabled); err != nil {
			return nil, err
		}
		out[id] = append(out[id], grant)
	}
	return out, rows.Err()
}

func (s *store) listJobs(ctx context.Context, f jobListFilter) ([]Job, *string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var b strings.Builder
	args := make([]any, 0, 8)
	b.WriteString(`SELECT j.id, j.installation_id, j.type, j.actor_type, j.actor_id, j.resource_type, j.resource_id,
	j.status, j.priority, j.attempts, j.max_attempts, j.run_after, j.last_error_code, j.last_error_message,
	j.created_at, j.updated_at, j.finished_at, i.account_id
FROM jobs j
LEFT JOIN installations i ON i.id = j.installation_id
WHERE 1=1`)
	if f.InstallationID != nil {
		fmt.Fprintf(&b, " AND j.installation_id = $%d", len(args)+1)
		args = append(args, *f.InstallationID)
	}
	if f.Status != "" {
		fmt.Fprintf(&b, " AND j.status = $%d", len(args)+1)
		args = append(args, f.Status)
	}
	if f.Type != "" {
		fmt.Fprintf(&b, " AND j.type = $%d", len(args)+1)
		args = append(args, f.Type)
	}
	if f.Since != nil {
		fmt.Fprintf(&b, " AND j.created_at >= $%d", len(args)+1)
		args = append(args, *f.Since)
	}
	if f.CursorTime != nil && f.CursorID != "" {
		id, err := uuid.Parse(f.CursorID)
		if err != nil {
			return nil, nil, errInvalid("invalid cursor")
		}
		fmt.Fprintf(&b, " AND (j.updated_at, j.id) < ($%d::timestamptz, $%d::uuid)", len(args)+1, len(args)+2)
		args = append(args, *f.CursorTime, id)
	}
	fmt.Fprintf(&b, " ORDER BY j.updated_at DESC, j.id DESC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]Job, 0)
	for rows.Next() {
		item, err := scanJob(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[len(items)-1]
		cursor := encodeCursor(last.UpdatedAt, last.ID.String())
		next = &cursor
	}
	return items, next, nil
}

func (s *store) getJob(ctx context.Context, id uuid.UUID) (Job, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	row := s.pool.QueryRow(ctx, `SELECT j.id, j.installation_id, j.type, j.actor_type, j.actor_id, j.resource_type, j.resource_id,
	j.status, j.priority, j.attempts, j.max_attempts, j.run_after, j.last_error_code, j.last_error_message,
	j.created_at, j.updated_at, j.finished_at, i.account_id
FROM jobs j
LEFT JOIN installations i ON i.id = j.installation_id
WHERE j.id=$1`, id)
	item, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, errNotFound("job not found")
	}
	return item, err
}

func (s *store) listJobAttempts(ctx context.Context, jobID uuid.UUID) ([]JobAttempt, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT id, job_id, attempt, worker_id, started_at, finished_at, outcome, error_code, error_message, duration_ms
FROM job_attempts WHERE job_id=$1 ORDER BY attempt`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]JobAttempt, 0)
	for rows.Next() {
		var item JobAttempt
		if err := rows.Scan(
			&item.ID, &item.JobID, &item.Attempt, &item.WorkerID, &item.StartedAt, &item.FinishedAt,
			&item.Outcome, &item.ErrorCode, &item.ErrorMessage, &item.DurationMS,
		); err != nil {
			return nil, err
		}
		item.StartedAt = item.StartedAt.UTC()
		if item.FinishedAt != nil {
			utc := item.FinishedAt.UTC()
			item.FinishedAt = &utc
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *store) jobsSummary(ctx context.Context) (map[string]int, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[status] = n
	}
	return counts, rows.Err()
}

func (s *store) listAudit(ctx context.Context, f auditListFilter) ([]AuditEntry, *string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var b strings.Builder
	args := make([]any, 0, 8)
	b.WriteString(`SELECT id, installation_id, actor_type, actor_id, action, object_type, object_id, metadata, correlation_job_id, created_at
FROM audit_log
WHERE 1=1`)
	if f.InstallationID != nil {
		fmt.Fprintf(&b, " AND installation_id = $%d", len(args)+1)
		args = append(args, *f.InstallationID)
	}
	if f.ObjectType != "" {
		fmt.Fprintf(&b, " AND object_type = $%d", len(args)+1)
		args = append(args, f.ObjectType)
	}
	if f.ObjectID != "" {
		fmt.Fprintf(&b, " AND object_id = $%d", len(args)+1)
		args = append(args, f.ObjectID)
	}
	if f.Action != "" {
		fmt.Fprintf(&b, " AND action = $%d", len(args)+1)
		args = append(args, f.Action)
	}
	if f.CursorTime != nil && f.CursorID != "" {
		id, err := strconv.ParseInt(f.CursorID, 10, 64)
		if err != nil {
			return nil, nil, errInvalid("invalid cursor")
		}
		fmt.Fprintf(&b, " AND (created_at, id) < ($%d::timestamptz, $%d::bigint)", len(args)+1, len(args)+2)
		args = append(args, *f.CursorTime, id)
	}
	fmt.Fprintf(&b, " ORDER BY created_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]AuditEntry, 0)
	for rows.Next() {
		var item AuditEntry
		if err := rows.Scan(
			&item.ID, &item.InstallationID, &item.ActorType, &item.ActorID, &item.Action,
			&item.ObjectType, &item.ObjectID, &item.Metadata, &item.CorrelationJobID, &item.CreatedAt,
		); err != nil {
			return nil, nil, err
		}
		item.CreatedAt = item.CreatedAt.UTC()
		if len(item.Metadata) == 0 {
			item.Metadata = json.RawMessage(`{}`)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(items) > f.Limit {
		items = items[:f.Limit]
		last := items[len(items)-1]
		cursor := encodeCursor(last.CreatedAt, strconv.FormatInt(last.ID, 10))
		next = &cursor
	}
	return items, next, nil
}

func (s *store) listIntegrations(ctx context.Context, limit int, cursorTime *time.Time, cursorID string) ([]Integration, *string, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var b strings.Builder
	args := make([]any, 0, 4)
	b.WriteString(`SELECT id, code, client_id, redirect_uri, status, webhook_events, created_at, updated_at, client_secret_key_version
FROM integrations
WHERE 1=1`)
	if cursorTime != nil && cursorID != "" {
		id, err := uuid.Parse(cursorID)
		if err != nil {
			return nil, nil, errInvalid("invalid cursor")
		}
		fmt.Fprintf(&b, " AND (updated_at, id) < ($%d::timestamptz, $%d::uuid)", len(args)+1, len(args)+2)
		args = append(args, *cursorTime, id)
	}
	fmt.Fprintf(&b, " ORDER BY updated_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, limit+1)
	rows, err := s.pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]Integration, 0)
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		item, err := scanIntegration(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		ids = ids[:limit]
		last := items[len(items)-1]
		cursor := encodeCursor(last.UpdatedAt, last.ID.String())
		next = &cursor
	}
	if err := s.attachIntegrationExtras(ctx, items, ids); err != nil {
		return nil, nil, err
	}
	return items, next, nil
}

func (s *store) getIntegration(ctx context.Context, id uuid.UUID) (Integration, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	row := s.pool.QueryRow(ctx, `SELECT id, code, client_id, redirect_uri, status, webhook_events, created_at, updated_at, client_secret_key_version
FROM integrations WHERE id=$1`, id)
	item, err := scanIntegration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Integration{}, errNotFound("integration not found")
	}
	if err != nil {
		return Integration{}, err
	}
	items := []Integration{item}
	if err := s.attachIntegrationExtras(ctx, items, []uuid.UUID{item.ID}); err != nil {
		return Integration{}, err
	}
	return items[0], nil
}

func (s *store) attachIntegrationExtras(ctx context.Context, items []Integration, ids []uuid.UUID) error {
	if len(items) == 0 {
		return nil
	}
	grants, err := s.grantsByIntegration(ctx, ids)
	if err != nil {
		return err
	}
	counts, err := s.installationCounts(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		g := grants[items[i].ID]
		if g == nil {
			g = []Grant{}
		}
		items[i].Grants = g
		c := counts[items[i].ID]
		if c == nil {
			c = map[string]int{}
		}
		items[i].InstallationsByStatus = c
	}
	return nil
}

func (s *store) installationCounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[string]int, error) {
	out := make(map[uuid.UUID]map[string]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT integration_id, status, count(*)
FROM installations WHERE integration_id = ANY($1) GROUP BY integration_id, status`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var status string
		var n int
		if err := rows.Scan(&id, &status, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]int)
		}
		out[id][status] = n
	}
	return out, rows.Err()
}

type jobScanner interface {
	Scan(dest ...any) error
}

func scanJob(row jobScanner) (Job, error) {
	var item Job
	err := row.Scan(
		&item.ID, &item.InstallationID, &item.Type, &item.ActorType, &item.ActorID,
		&item.ResourceType, &item.ResourceID, &item.Status, &item.Priority, &item.Attempts,
		&item.MaxAttempts, &item.RunAfter, &item.LastErrorCode, &item.LastErrorMessage,
		&item.CreatedAt, &item.UpdatedAt, &item.FinishedAt, &item.AccountID,
	)
	if err != nil {
		return Job{}, err
	}
	item.RunAfter = item.RunAfter.UTC()
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	if item.FinishedAt != nil {
		utc := item.FinishedAt.UTC()
		item.FinishedAt = &utc
	}
	return item, nil
}

func scanIntegration(row jobScanner) (Integration, error) {
	var item Integration
	var events []byte
	err := row.Scan(
		&item.ID, &item.Code, &item.ClientID, &item.RedirectURI, &item.Status, &events,
		&item.CreatedAt, &item.UpdatedAt, &item.KeyVersion,
	)
	if err != nil {
		return Integration{}, err
	}
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	item.WebhookEvents = decodeStringArray(events)
	item.Grants = []Grant{}
	item.InstallationsByStatus = map[string]int{}
	return item, nil
}

func decodeStringArray(raw []byte) []string {
	out := make([]string, 0)
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}

func appendInstallationFilters(b *strings.Builder, args *[]any, f installationListFilter) {
	appendInstallationFiltersShifted(b, args, f)
}

func appendInstallationFiltersShifted(b *strings.Builder, args *[]any, f installationListFilter) {
	add := func(clause string, value any) {
		fmt.Fprintf(b, clause, len(*args)+1)
		*args = append(*args, value)
	}
	if f.AccountID != nil {
		add(" AND i.account_id = $%d", *f.AccountID)
	}
	if f.Domain != "" {
		add(" AND lower(i.account_domain) = lower($%d)", f.Domain)
	}
	if f.IntegrationID != nil {
		add(" AND i.integration_id = $%d", *f.IntegrationID)
	}
	if f.Status != "" {
		add(" AND i.status = $%d", f.Status)
	}
	if f.WebhookStatus != "" {
		add(" AND i.webhook_status = $%d", f.WebhookStatus)
	}
	q := f.Query
	if q.AccountID != nil {
		add(" AND i.account_id = $%d", *q.AccountID)
	}
	if q.Domain != "" {
		add(" AND lower(i.account_domain) = $%d", q.Domain)
	}
	if q.Subdomain != "" {
		n := len(*args) + 1
		fmt.Fprintf(b, ` AND (
			lower(i.account_domain) = $%d
			OR lower(i.account_domain) = $%d || '.amocrm.ru'
			OR lower(i.account_domain) = $%d || '.kommo.com'
			OR lower(i.account_domain) = $%d || '.amocrm.test'
			OR lower(i.account_domain) = $%d || '.kommo.test'
		)`, n, n, n, n, n)
		*args = append(*args, q.Subdomain)
	}
}
