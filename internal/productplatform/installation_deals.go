package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sk1fy/amocrm-pro/internal/companycrm"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

const (
	InstallationDealJob          queue.Kind = "installation.deal"
	InstallationDealUsersTimeout            = 5 * time.Second
	dealTechnicalAccount                    = "deal_technical_account"
	dealTariffUnknown                       = "deal_tariff_unknown"
)

type InstallationDealSource interface {
	Scope() (int64, string)
	SendInstallationDeal(context.Context, companycrm.DealRequest) (companycrm.DealResult, error)
}

type InstallationDeals struct {
	Pool               *pgxpool.Pool
	Source             InstallationDealSource
	Manager            *ManagerOptions
	Accounts           *AccountObserver
	RequireKnownTariff bool
}

func (d InstallationDeals) Run(ctx context.Context, job queue.Job) queue.Completion {
	if d.Accounts != nil {
		// Refresh before reserving the write so an interrupted read cannot leave a deal on reconciliation.
		refreshCtx, cancel := context.WithTimeout(ctx, InstallationDealUsersTimeout)
		err := d.Accounts.refreshInstallationDealUsers(refreshCtx, job.ID, d.RequireKnownTariff)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Installation deal user count unavailable")
		}
	}
	request, skip, err := d.reserve(ctx, job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return queue.Completion{}
	}
	if err != nil {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "deal_service_unavailable", Retry: true}}
	}
	if skip == dealTariffUnknown {
		return d.deferForTariff(job.ID)
	}
	if skip != "" {
		return d.publish(job.ID, skip, companycrm.DealResult{}, queue.Outcome{})
	}
	result, err := d.Source.SendInstallationDeal(ctx, request)
	if err != nil {
		return d.publish(job.ID, "reconciling", result, queue.Outcome{
			FailureCode: "deal_outcome_unknown", Retry: true, RetryAfter: time.Minute,
		})
	}
	switch result.State {
	case "found":
		return d.publish(job.ID, "succeeded", result, queue.Outcome{})
	case "rejected":
		return d.publish(job.ID, "failed", result, queue.Outcome{FailureCode: "deal_rejected"})
	case "rate_limited":
		return d.publish(job.ID, "pending", result, queue.Outcome{
			FailureCode: "deal_rate_limited", Retry: true, RetryAfter: time.Minute,
		})
	default:
		return d.publish(job.ID, "reconciling", result, queue.Outcome{
			FailureCode: "deal_not_found", Retry: true, RetryAfter: time.Minute,
		})
	}
}

func (o *AccountObserver) refreshInstallationDealUsers(ctx context.Context, episode uuid.UUID, requireKnownTariff bool) error {
	var account uuid.UUID
	err := o.pool.QueryRow(ctx, `
		SELECT i.account_id FROM platform_manager.installation_deals d
		JOIN platform_manager.installations i ON i.id=d.episode_id
		LEFT JOIN platform_manager.account_profiles p ON p.account_id=i.account_id
		LEFT JOIN platform_manager.account_tariff_fields t ON t.account_id=i.account_id AND t.field='tariff_id'
		WHERE d.episode_id=$1 AND d.status='pending' AND d.title IS NULL AND i.lifecycle='current' AND i.kind='install'
			AND p.classification IS DISTINCT FROM 'Запрет работы'
			AND t.numeric_value IS DISTINCT FROM $2 AND (NOT $3 OR t.observed_at>=i.opened_at)
			AND NOT EXISTS(
				SELECT FROM platform_manager.account_rosters r
				WHERE r.account_id=i.account_id AND r.failure_code IS NULL AND r.fresh_until>statement_timestamp()
			)`, episode, technicalAccountTariffID, requireKnownTariff).Scan(&account)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	scope, err := accountSourceScope(ctx, o.pool, account)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	observation, outcome := observeAccount(ctx, o.gateway, scope, refreshUsers)
	if outcome.FailureCode != "" {
		return nil
	}
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := publishAccountObservation(ctx, tx, accountRefreshPayload{Account: account, Component: refreshUsers}, scope, observation, outcome, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d InstallationDeals) reserve(ctx context.Context, episode uuid.UUID) (companycrm.DealRequest, string, error) {
	request := companycrm.DealRequest{Episode: episode}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return request, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var clientAccount, installation uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT p.account_id,e.installation_id FROM platform_manager.installation_deals d
		JOIN platform_lifecycle.episodes e ON e.id=d.episode_id
		JOIN platform_lifecycle.account_products p ON p.installation_id=e.installation_id
		WHERE d.episode_id=$1`, episode).Scan(&clientAccount, &installation)
	if err != nil {
		return request, "", err
	}
	if _, err := lockStream(ctx, tx, installation); err != nil {
		return request, "", err
	}
	classification, err := lockAccountClassification(ctx, tx, clientAccount)
	if err != nil {
		return request, "", err
	}
	var status string
	var title, previousBinding *string
	var previousAccount *int64
	var generation int64
	err = tx.QueryRow(ctx, `
		SELECT d.status,d.title,d.company_account,d.binding_revision,e.generation
		FROM platform_manager.installation_deals d
		JOIN platform_lifecycle.episodes e ON e.id=d.episode_id
		WHERE d.episode_id=$1 FOR UPDATE OF d`, episode).
		Scan(&status, &title, &previousAccount, &previousBinding, &generation)
	if err != nil {
		return request, "", err
	}
	if status == "succeeded" || status == "skipped" || status == "failed" {
		return request, "", pgx.ErrNoRows
	}
	account, revision := d.Source.Scope()
	request.Account, request.Binding = account, revision
	if title != nil {
		if *previousAccount != account || *previousBinding != revision {
			return request, "reconciling", nil
		}
		request.Title = *title
		return request, "", tx.Commit(ctx)
	}
	if generation > 1 {
		return request, "skipped", nil
	}

	var name, accountLabel, contactName, lifecycle string
	var installedAt time.Time
	var tariffID *int64
	var tariffObservedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT inst.product_name,COALESCE(i.current_domain,inst.amo_account_id::text),
			COALESCE(c.name,''),inst.lifecycle,inst.opened_at,inst.amo_account_id,u.active_count,t.numeric_value,t.observed_at
		FROM platform_manager.installations inst
		LEFT JOIN platform_manager.account_identities i USING(account_id)
		LEFT JOIN platform_manager.account_rosters u
			ON u.account_id=inst.account_id AND u.failure_code IS NULL AND u.fresh_until>transaction_timestamp()
		LEFT JOIN platform_manager.installation_contacts c ON c.episode_id=inst.id
		LEFT JOIN platform_manager.account_tariff_fields t ON t.account_id=inst.account_id AND t.field='tariff_id'
		WHERE inst.id=$1`, episode).Scan(&name, &accountLabel, &contactName, &lifecycle, &installedAt,
		&request.ClientAccountID, &request.ActiveUsers, &tariffID, &tariffObservedAt)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && lifecycle != "current" {
		return request, "skipped", nil
	}
	if err != nil {
		return request, "", err
	}
	if tariffID != nil && *tariffID == technicalAccountTariffID {
		return request, dealTechnicalAccount, nil
	}
	if classification == "Запрет работы" {
		return request, "skipped", nil
	}
	if d.RequireKnownTariff && (tariffObservedAt == nil || tariffObservedAt.Before(installedAt)) {
		return request, dealTariffUnknown, nil
	}
	var pipeline, stage int64
	err = tx.QueryRow(ctx, `
		SELECT r.pipeline_id,r.stage_id,COALESCE((r.owners->>$3::text)::bigint,0)
		FROM platform_company.source_control c
		JOIN LATERAL (
			SELECT pipeline_id,stage_id,owners FROM platform_company.installation_deal_settings_revisions
			WHERE scope_id=c.scope_id ORDER BY revision DESC LIMIT 1
		) r ON true
		JOIN platform_company.source_scopes s ON s.id=c.scope_id
		JOIN platform_company.source_components m ON m.scope_id=s.id AND m.component='metadata'
		JOIN platform_company.generations metadata ON metadata.id=m.current_generation
		JOIN platform_company.workflow_stages g ON g.generation_id=m.current_generation
			AND g.pipeline_id=r.pipeline_id AND g.stage_id=r.stage_id
		WHERE s.account_id=$1 AND s.binding_revision=$2
          AND (NOT (r.owners ? $3::text) OR EXISTS (
            SELECT FROM jsonb_array_elements(metadata.employees) employee
            WHERE employee->>'id'=r.owners->>$3::text))`, account, revision, installationDealOwnerKey(classification)).Scan(&pipeline, &stage, &request.ResponsibleUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return request, "failed", nil
	}
	if err != nil {
		return request, "", err
	}
	date := installedAt.UTC()
	if d.Manager != nil {
		date = date.In(d.Manager.zone)
	}
	request.Title = installationDealTitle(name, accountLabel, contactName, date)
	request.Pipeline, request.Stage, request.Create = pipeline, stage, true
	_, err = tx.Exec(ctx, `
		UPDATE platform_manager.installation_deals
		SET status='reconciling',title=$2,company_account=$3,binding_revision=$4,reason=NULL
		WHERE episode_id=$1`, episode, request.Title, account, revision)
	if err != nil {
		return request, "", err
	}
	return request, "", tx.Commit(ctx)
}

func installationDealTitle(product, accountLabel, contact string, installedAt time.Time) string {
	account, _, _ := strings.Cut(accountLabel, ".")
	label := product + " · " + account
	if contact = strings.TrimSpace(contact); contact != "" {
		label += " · " + contact
	}
	suffix := " · " + installedAt.Format("02.01.2006")
	var trimmed strings.Builder
	for _, r := range label {
		if trimmed.Len()+len(string(r))+len(suffix) > 512 {
			break
		}
		trimmed.WriteRune(r)
	}
	return strings.TrimSpace(trimmed.String()) + suffix
}

func (d InstallationDeals) publish(episode uuid.UUID, state string, result companycrm.DealResult, outcome queue.Outcome) queue.Completion {
	if state == "failed" && outcome.FailureCode == "" {
		outcome.FailureCode = "deal_workflow_unavailable"
	}
	if state == "reconciling" && outcome.FailureCode == "" {
		outcome.FailureCode = "deal_binding_changed"
	}
	reason := outcome.FailureCode
	if state == dealTechnicalAccount {
		state, reason = "skipped", dealTechnicalAccount
	}
	return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
		_, err := tx.Exec(ctx, `
			UPDATE platform_manager.installation_deals
			SET status=$2, deal_id=NULLIF($3,0),deal_host=NULLIF($4,''),reason=NULLIF($5,''),
			    title=CASE WHEN $2 IN ('failed','pending') THEN NULL ELSE title END,
			    company_account=CASE WHEN $2 IN ('failed','pending') THEN NULL ELSE company_account END,
			    binding_revision=CASE WHEN $2 IN ('failed','pending') THEN NULL ELSE binding_revision END
		WHERE episode_id=$1`, episode, state, result.ID, result.Host, reason)
		return outcome, err
	}}
}

func dealURL(id *int64, host *string) *string {
	if id == nil || host == nil {
		return nil
	}
	return new("https://" + *host + "/leads/detail/" + strconv.FormatInt(*id, 10))
}

type installationContact struct {
	UserID string  `json:"amo_user_id"`
	Name   string  `json:"name"`
	Phone  string  `json:"phone"`
	Email  *string `json:"email,omitempty"`
	Role   *string `json:"role,omitempty"`
}

type installationDeal struct {
	State        string     `json:"state"`
	URL          *string    `json:"url,omitempty"`
	ID           *string    `json:"id,omitempty"`
	Reason       *string    `json:"reason,omitempty"`
	NextRun      *time.Time `json:"next_run_at,omitempty"`
	RetryVersion int        `json:"retry_version"`
	CanRetry     bool       `json:"can_retry"`
}

const dealRetryKind = "installation.crm_deal.retry"

type dealRetryCommand struct {
	ID, Subject, Key, Episode uuid.UUID
	Version                   int64
	Reason                    string
}

func (s managerStore) retryInstallationDeal(ctx context.Context, cmd dealRetryCommand) (accountOperation, error) {
	var op accountOperation
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return op, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var jobStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM platform_jobs.jobs WHERE id=$1 AND kind='installation.deal' FOR UPDATE`, cmd.Episode).Scan(&jobStatus)
	if err != nil {
		return op, parseDealRetryError(err)
	}
	var current int64
	var state string
	err = tx.QueryRow(ctx, `
		SELECT d.status,d.retry_version FROM platform_manager.installation_deals d
		JOIN platform_manager.installations i ON i.id=d.episode_id
		WHERE d.episode_id=$1 FOR UPDATE OF d`, cmd.Episode).Scan(&state, &current)
	if err != nil {
		return op, parseDealRetryError(err)
	}
	var at time.Time
	err = tx.QueryRow(ctx, `
		SELECT created_at FROM platform_manager.installation_deal_retries
		WHERE id=$1 AND subject_id=$2 AND idempotency_key=$3 AND episode_id=$4 AND version=$5 AND reason=$6`,
		cmd.ID, cmd.Subject, cmd.Key, cmd.Episode, cmd.Version, cmd.Reason).Scan(&at)
	if err == nil {
		return dealOperation(cmd.ID, cmd.Episode, "accepted", at, at), tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return op, err
	}
	if cmd.Version != current || jobStatus != "dead" || state == "skipped" || state == "succeeded" || current >= 2147483647 {
		return op, errManagerConflict
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO platform_manager.installation_deal_retries(id,subject_id,idempotency_key,episode_id,version,reason)
		VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING RETURNING created_at`,
		cmd.ID, cmd.Subject, cmd.Key, cmd.Episode, cmd.Version, cmd.Reason).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return op, errManagerConflict
	}
	if err != nil {
		return op, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE platform_manager.installation_deals
		SET status=CASE WHEN status='failed' THEN 'pending' ELSE status END, retry_version=retry_version+1,
		    reason=NULL
		WHERE episode_id=$1`, cmd.Episode)
	if err != nil {
		return op, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE platform_jobs.jobs SET status='queued',attempt=0,available_at=statement_timestamp(),
		    updated_at=statement_timestamp(),failure_code=NULL WHERE id=$1`, cmd.Episode)
	if err != nil {
		return op, err
	}
	return dealOperation(cmd.ID, cmd.Episode, "accepted", at, at), tx.Commit(ctx)
}

func dealOperation(id, episode uuid.UUID, state string, created, updated time.Time) accountOperation {
	return accountOperation{ID: id, Kind: dealRetryKind, State: state,
		Target: accountOperationTarget{Kind: "installation", ID: episode}, CreatedAt: created.UTC(), UpdatedAt: updated.UTC()}
}

func (s managerStore) readDealOperation(ctx context.Context, id, subject uuid.UUID) (accountOperation, error) {
	var episode uuid.UUID
	var state string
	var created, updated time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT r.episode_id, r.created_at, GREATEST(j.updated_at,r.created_at),
		CASE
		  WHEN d.retry_version > r.version+1 THEN 'failed'
		  WHEN j.status='running' THEN 'running'
		  WHEN j.status='queued' THEN 'waiting_external'
		  WHEN d.status='succeeded' THEN 'succeeded'
		  WHEN d.status='reconciling' THEN 'needs_review'
		  ELSE 'failed' END
		FROM platform_manager.installation_deal_retries r
		JOIN platform_manager.installations i ON i.id=r.episode_id
		JOIN platform_manager.installation_deals d ON d.episode_id=r.episode_id
		JOIN platform_jobs.jobs j ON j.id=r.episode_id
		WHERE r.id=$1 AND r.subject_id=$2`, id, subject).Scan(&episode, &created, &updated, &state)
	return dealOperation(id, episode, state, created, updated), parseDealRetryError(err)
}

func parseDealRetryError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errManagerNotFound
	}
	return err
}

const InstallationDealTitleJob queue.Kind = "installation.deal.title"

func (d InstallationDeals) SyncTitle(send func(context.Context, companycrm.DealTitleRequest) error) func(context.Context, queue.Job) queue.Completion {
	return func(ctx context.Context, job queue.Job) queue.Completion {
		var payload struct {
			Episode uuid.UUID `json:"episode_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.Episode == uuid.Nil {
			return queue.Completion{Outcome: queue.Outcome{FailureCode: "invalid_payload"}}
		}
		var in companycrm.DealTitleRequest
		err := d.Pool.QueryRow(ctx, `SELECT d.company_account,d.binding_revision,d.deal_id,
 CASE WHEN e.generation>1 THEN 'reinstalled' WHEN EXISTS (SELECT FROM platform_lifecycle.history h WHERE h.episode_id=d.episode_id AND h.kind='reauthorized') THEN 'reauthorized' ELSE '' END
 FROM platform_manager.installation_deals d JOIN platform_lifecycle.episodes e ON e.id=d.episode_id WHERE d.episode_id=$1 AND d.status='succeeded'`, payload.Episode).Scan(&in.Account, &in.Binding, &in.DealID, &in.Event)
		if err == nil && in.Event != "" {
			err = send(ctx, in)
		}
		if err != nil {
			return queue.Completion{Outcome: queue.Outcome{FailureCode: "deal_title_unavailable", Retry: true, RetryAfter: time.Minute}}
		}
		return queue.Completion{}
	}
}
