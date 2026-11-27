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

	"github.com/sk1fy/amocrm-pro/internal/productplatform/amostart"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/platformpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	AccountRefreshJob        queue.Kind = "accounts.refresh"
	rosterFreshness                     = 24 * time.Hour
	accountSourceChanged                = "source_scope_changed"
	accountSourceUnavailable            = "source_unavailable"
	accountPaymentRequired              = "source_payment_required"
)

type AccountObserver struct {
	amoStart  *amostart.Client
	directory platformpb.CoreAccountDirectoryClient
	pool      *pgxpool.Pool
	gateway   platformpb.CoreAccountObservationsClient
}

func NewAccountObserver(pool *pgxpool.Pool, gateway platformpb.CoreAccountObservationsClient) *AccountObserver {
	return &AccountObserver{pool: pool, gateway: gateway}
}

func (o *AccountObserver) SetDirectory(client platformpb.CoreAccountDirectoryClient) {
	o.directory = client
}

func (o *AccountObserver) SetAmoStart(client *amostart.Client) {
	o.amoStart = client
}

type sourceScope struct {
	ObservationOnly                   bool
	AmoAccount                        int64
	Installation, Integration         uuid.UUID
	Generation, AuthorizationSequence int64
}

const validAccountObservationScope = `current AND effective_authorization='authorized' AND generation>0 AND authorization_sequence>0`

type accountScopeReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func accountSourceScope(ctx context.Context, db accountScopeReader, account uuid.UUID) (sourceScope, error) {
	return nextAccountSource(ctx, db, account, uuid.Nil)
}

func nextAccountSource(ctx context.Context, db accountScopeReader, account, after uuid.UUID) (sourceScope, error) {
	var scope sourceScope
	err := db.QueryRow(ctx, `
 SELECT amo_account_id,installation_id,integration_id,generation,authorization_sequence,observation_only
 FROM platform_manager.account_read_sources m
 WHERE account_id=$1 AND installation_id>$2
 AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p WHERE p.account_id=m.account_id AND p.classification='Запрет работы')
 ORDER BY installation_id LIMIT 1`, account, after).Scan(&scope.AmoAccount, &scope.Installation, &scope.Integration, &scope.Generation, &scope.AuthorizationSequence, &scope.ObservationOnly)
	return scope, err
}

type accountRefreshPayload struct {
	Account   uuid.UUID        `json:"account_id"`
	Component accountComponent `json:"component"`
}

type accountIdentityObservation struct {
	WidgetVersions map[string]string
	Domain, Name   string
	TimeZone       *string
}

type accountCRMUser struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"is_active"`
	Admin  bool   `json:"is_admin"`
}

type accountObservation struct {
	At              time.Time
	Identity        *accountIdentityObservation
	Roster          []accountCRMUser
	Active          int
	PaymentRequired bool
}

func (o *AccountObserver) RefreshJob(ctx context.Context, job queue.Job) queue.Completion {
	var p accountRefreshPayload
	if json.Unmarshal(job.Payload, &p) != nil || p.Account == uuid.Nil || (!p.Component.valid() && p.Component != refreshInstallations) {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "invalid_refresh_job"}}
	}
	var scope sourceScope
	var observation accountObservation
	var inventory []inventoryEntry
	var outcome queue.Outcome
	var amoStart amoStartObservation
	switch p.Component {
	case refreshInstallations:
		inventory, outcome = o.readInventory(ctx, p.Account)
	case refreshAmoStart:
		amoStart, outcome = o.readAmoStart(ctx, p.Account)
	default:
		scope, observation, outcome = o.readAccountComponent(ctx, p)
	}
	return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
		var err error
		switch p.Component {
		case refreshAmoStart:
			if outcome.FailureCode == "" {
				outcome, err = publishAmoStart(ctx, tx, p.Account, amoStart)
			}
		case refreshInstallations:
			if outcome.FailureCode == "" {
				outcome, err = publishInventory(ctx, tx, p.Account, inventory)
			}
		default:
			outcome, err = publishAccountObservation(ctx, tx, p, scope, observation, outcome, true)
		}
		if err != nil {
			return outcome, err
		}
		var paymentRequiredAt *time.Time
		if outcome.FailureCode == accountPaymentRequired {
			paymentRequiredAt = &observation.At
		}
		_, err = tx.Exec(ctx, `UPDATE platform_manager.account_refresh_slots SET attempted_at=statement_timestamp(),
      result_code=NULLIF($3,''), succeeded_at=CASE WHEN $3='' THEN statement_timestamp() ELSE succeeded_at END,
      payment_required_at=GREATEST(payment_required_at,$4::timestamptz)
      WHERE account_id=$1 AND component=$2`, p.Account, p.Component, outcome.FailureCode, paymentRequiredAt)
		return outcome, err
	}}
}

func (o *AccountObserver) readAccountComponent(ctx context.Context, p accountRefreshPayload) (sourceScope, accountObservation, queue.Outcome) {
	var scope sourceScope
	var observation accountObservation
	var outcome queue.Outcome
	var blocked bool
	if err := o.pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.account_profiles WHERE account_id=$1 AND classification='Запрет работы')`, p.Account).Scan(&blocked); err != nil {
		return scope, observation, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
	if blocked {
		return scope, observation, queue.Outcome{FailureCode: "account_blocked"}
	}
	for {
		next, err := nextAccountSource(ctx, o.pool, p.Account, scope.Installation)
		if errors.Is(err, pgx.ErrNoRows) {
			if outcome.FailureCode == "" {
				outcome = o.missingSource(ctx, p.Account)
			}
			return scope, observation, outcome
		}
		if err != nil {
			return scope, observation, queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
		}
		scope = next
		observation, outcome = observeAccount(ctx, o.gateway, scope, p.Component)
		if outcome.FailureCode != "source_permission_denied" && outcome.FailureCode != accountSourceChanged {
			return scope, observation, outcome
		}
	}
}

func (o *AccountObserver) missingSource(ctx context.Context, account uuid.UUID) queue.Outcome {
	var needsAuthorization bool
	err := o.pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.installation_directory d
 WHERE d.account_id=$1 AND d.lifecycle='current' AND d.authorization IN ('disabled','reauth_required'))`, account).Scan(&needsAuthorization)
	if err != nil {
		return queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
	if needsAuthorization {
		return queue.Outcome{FailureCode: "source_permission_denied"}
	}
	return queue.Outcome{FailureCode: accountSourceChanged}
}

func observeAccount(ctx context.Context, gateway platformpb.CoreAccountObservationsClient, scope sourceScope,
	part accountComponent) (accountObservation, queue.Outcome) {
	component := platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_LICENSE
	if part == refreshUsers {
		component = platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS
	}
	response, err := gateway.ObserveAccount(ctx, &platformpb.ObserveAccountRequest{
		ObservationOnly: scope.ObservationOnly, AmoAccountId: scope.AmoAccount, CoreInstallationId: scope.Installation.String(), IntegrationId: scope.Integration.String(),
		Generation: uint64(scope.Generation), AuthorizationSequence: uint64(scope.AuthorizationSequence), Component: component,
	})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return accountObservation{}, accountGatewayOutcome(err)
	}
	observation, ok := decodeAccountObservation(response, part)
	if !ok {
		return accountObservation{}, queue.Outcome{FailureCode: "source_incomplete", Retry: true}
	}
	if observation.PaymentRequired {
		return observation, queue.Outcome{FailureCode: accountPaymentRequired}
	}
	return observation, queue.Outcome{}
}

func accountGatewayOutcome(err error) queue.Outcome {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return queue.Outcome{FailureCode: accountSourceChanged}
	case codes.FailedPrecondition:
		return queue.Outcome{FailureCode: "source_permission_denied"}
	case codes.ResourceExhausted:
		outcome := queue.Outcome{FailureCode: "source_rate_limited", Retry: true}
		for _, detail := range status.Convert(err).Details() {
			if retry, ok := detail.(*errdetails.RetryInfo); ok && retry.GetRetryDelay().CheckValid() == nil {
				outcome.RetryAfter = max(outcome.RetryAfter, retry.GetRetryDelay().AsDuration())
			}
		}
		return outcome
	case codes.DeadlineExceeded:
		return queue.Outcome{FailureCode: "source_timeout", Retry: true}
	default:
		if errors.Is(err, context.DeadlineExceeded) {
			return queue.Outcome{FailureCode: "source_timeout", Retry: true}
		}
		return queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}
	}
}

func decodeAccountObservation(response *platformpb.AccountObservation, part accountComponent) (accountObservation, bool) {
	timestamp := response.GetObservedAt()
	if timestamp.CheckValid() != nil {
		return accountObservation{}, false
	}
	at := timestamp.AsTime()
	if at.After(time.Now().Add(time.Minute)) {
		return accountObservation{}, false
	}
	out := accountObservation{At: at}
	if part == refreshLicense {
		if response.GetPaymentRequired() != nil {
			out.PaymentRequired = true
			return out, true
		}
		identity := response.GetIdentity()
		if identity == nil || !validObservedDomain(identity.CurrentDomain) || !managerText(identity.DisplayName, 1, 512) ||
			strings.TrimSpace(identity.DisplayName) == "" {
			return out, false
		}
		if zone := identity.TimeZone; zone != nil {
			if _, err := time.LoadLocation(*zone); err != nil || *zone == "" || *zone == "Local" || !managerText(*zone, 1, 128) {
				return out, false
			}
		}
		for id, v := range identity.WidgetVersions {
			if _, err := managerUUID(id); err != nil || !managerText(v, 1, 64) {
				return out, false
			}
		}
		out.Identity = &accountIdentityObservation{WidgetVersions: identity.WidgetVersions, Domain: identity.CurrentDomain, Name: identity.DisplayName, TimeZone: identity.TimeZone}
		return out, true
	}
	roster := response.GetRoster()
	if roster == nil || len(roster.Users) > 1000 {
		return out, false
	}
	out.Roster = make([]accountCRMUser, 0, len(roster.Users))
	seen := make(map[int64]bool, len(roster.Users))
	for _, user := range roster.Users {
		if user == nil || user.Id <= 0 || seen[user.Id] || !managerText(user.Name, 0, 512) {
			return out, false
		}
		seen[user.Id] = true
		if user.IsActive {
			out.Active++
		}
		out.Roster = append(out.Roster, accountCRMUser{
			ID: strconv.FormatInt(user.Id, 10), Name: user.Name, Active: user.IsActive, Admin: user.IsAdmin,
		})
	}
	return out, true
}

func validObservedDomain(domain string) bool {
	if len(domain) < 3 || len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for part := range strings.SplitSeq(domain, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func publishAccountObservation(ctx context.Context, tx pgx.Tx, p accountRefreshPayload, scope sourceScope,
	obs accountObservation, outcome queue.Outcome, checkSubscriptionSeats bool) (queue.Outcome, error) {
	if scope.ObservationOnly {
		var locked uuid.UUID
		err := tx.QueryRow(ctx, `SELECT installation_id FROM platform_manager.core_inventory WHERE account_id=$1 AND installation_id=$2 AND integration_id=$3 AND current AND auth_state='authorized' AND observation_only FOR SHARE`, p.Account, scope.Installation, scope.Integration).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = queue.Outcome{FailureCode: accountSourceChanged}
		} else if err != nil {
			return outcome, err
		}
	} else if scope.Installation != uuid.Nil {
		// Lock the cursor until commit so a local transition cannot invalidate an
		// authorization between the final check and publishing its observation.
		var installation uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT c.installation_id FROM platform_lifecycle.cursors c
			WHERE c.installation_id=$1 AND c.generation=$2 AND c.authorization_sequence=$3 AND c.effective_authorization='authorized'
			FOR SHARE`, scope.Installation, scope.Generation, scope.AuthorizationSequence).Scan(&installation)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return outcome, err
		}
		var approved bool
		if installation != uuid.Nil {
			err = tx.QueryRow(ctx, `SELECT EXISTS(
				SELECT FROM platform_manager.account_memberships WHERE account_id=$1 AND amo_account_id=$2
				AND installation_id=$3 AND integration_id=$4 AND `+validAccountObservationScope+`
			)`, p.Account, scope.AmoAccount, scope.Installation, scope.Integration).Scan(&approved)
			if err != nil {
				return outcome, err
			}
		}
		if !approved {
			outcome = queue.Outcome{FailureCode: accountSourceChanged}
		}
	}
	if outcome.FailureCode != "" {
		if p.Component == refreshUsers {
			_, err := tx.Exec(ctx, `
				INSERT INTO platform_manager.account_rosters(account_id,failure_code)
				SELECT $1,$2 WHERE EXISTS(SELECT FROM platform_manager.visible_accounts WHERE account_id=$1)
				ON CONFLICT(account_id) DO UPDATE SET failure_code=EXCLUDED.failure_code`, p.Account, outcome.FailureCode)
			return outcome, err
		}
		return outcome, nil
	}
	if p.Component == refreshUsers {
		_, err := tx.Exec(ctx, `
			WITH observed AS (
				INSERT INTO platform_manager.account_rosters AS roster(account_id,roster,active_count,evidence_id,observed_at,fresh_until)
				VALUES($1,$2,$3,$4,$5,$6)
				ON CONFLICT(account_id) DO UPDATE SET roster=EXCLUDED.roster,active_count=EXCLUDED.active_count,
					evidence_id=EXCLUDED.evidence_id,observed_at=EXCLUDED.observed_at,
					fresh_until=EXCLUDED.fresh_until,failure_code=NULL
				WHERE roster.observed_at IS NULL OR roster.observed_at<=EXCLUDED.observed_at
				RETURNING account_id,active_count,observed_at
			)
			INSERT INTO platform_manager.account_subscription_rosters AS previous(account_id,check_number,active_count,observed_at)
			SELECT account_id,1,active_count,observed_at FROM observed
			WHERE $7 AND $6::timestamptz>statement_timestamp()
			ON CONFLICT(account_id) DO UPDATE SET check_number=previous.check_number+1,
				active_count=EXCLUDED.active_count,observed_at=EXCLUDED.observed_at,previous_active_count=previous.active_count
			WHERE previous.observed_at IS NULL OR previous.observed_at<EXCLUDED.observed_at`,
			p.Account, obs.Roster, obs.Active, uuid.New(), obs.At, obs.At.Add(rosterFreshness), checkSubscriptionSeats)
		return outcome, err
	}
	if len(obs.Identity.WidgetVersions) > 0 {
		_, err := tx.Exec(ctx, `INSERT INTO platform_manager.installation_widget_versions(installation_id,generation,widget_version,observed_at)
             SELECT d.installation_id,d.generation,w.value,$3 FROM platform_manager.installation_directory d
             JOIN jsonb_each_text($2::jsonb) w ON d.installation_id=w.key::uuid
             WHERE d.account_id=$1 AND d.lifecycle='current' AND d.opened_at<=$3
             ON CONFLICT(installation_id,generation) DO UPDATE SET widget_version=EXCLUDED.widget_version,observed_at=EXCLUDED.observed_at
             WHERE platform_manager.installation_widget_versions.observed_at<=EXCLUDED.observed_at`, p.Account, obs.Identity.WidgetVersions, obs.At)
		if err != nil {
			return outcome, err
		}
	}
	_, err := tx.Exec(ctx, `
		WITH observed AS (
			INSERT INTO platform_manager.account_identities(account_id,current_domain,display_name,time_zone,observed_at)
			VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(account_id) DO UPDATE SET current_domain=EXCLUDED.current_domain,
				display_name=EXCLUDED.display_name,time_zone=EXCLUDED.time_zone,observed_at=EXCLUDED.observed_at
			WHERE platform_manager.account_identities.observed_at<=EXCLUDED.observed_at
			RETURNING account_id,current_domain
		)
		INSERT INTO platform_manager.account_domains(account_id,domain)
		SELECT account_id,current_domain FROM observed ON CONFLICT DO NOTHING`, p.Account,
		obs.Identity.Domain, obs.Identity.Name, obs.Identity.TimeZone, obs.At)
	return outcome, err
}

func (o *AccountObserver) Enroll(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var nextAmoStartScan time.Time
	for {
		now := time.Now()
		scanAmoStart := o.amoStart != nil && !now.Before(nextAmoStartScan)
		if scanAmoStart {
			nextAmoStartScan = now.Add(15 * time.Second)
		}
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := o.enroll(queryCtx, scanAmoStart)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Account observation enrollment failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *AccountObserver) enroll(ctx context.Context, scanAmoStart bool) error {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1579107215,3)`).Scan(&locked); err != nil || !locked {
		return err
	}
	var queued int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM platform_jobs.jobs WHERE kind='accounts.refresh' AND status IN ('queued','running')`).Scan(&queued); err != nil {
		return err
	}
	limit := min(8, 32-queued)
	if limit <= 0 {
		return nil
	}
	if scanAmoStart {
		added, err := enrollAmoStart(ctx, tx)
		if err != nil {
			return err
		}
		if added {
			limit = min(limit, 31-queued)
		}
	}
	rows, err := tx.Query(ctx, `
 SELECT a.account_id,part.component
 FROM platform_manager.visible_accounts a CROSS JOIN (VALUES ('installations'),('license'),('users')) AS part(component)
 LEFT JOIN platform_manager.account_refresh_slots s ON s.account_id=a.account_id AND s.component=part.component
 LEFT JOIN platform_jobs.jobs j ON j.id=s.job_id
 LEFT JOIN LATERAL (SELECT CASE WHEN NOT m.observation_only THEN m.installation_id END AS installation_id,m.generation,m.authorization_sequence FROM platform_manager.account_read_sources m
     WHERE m.account_id=a.account_id ORDER BY m.installation_id LIMIT 1) src ON true
 WHERE (part.component<>'installations' OR $2)
 AND (part.component='installations' OR NOT $2 OR src.installation_id IS NOT NULL OR EXISTS(
      SELECT FROM platform_manager.account_refresh_slots done JOIN platform_jobs.jobs dj ON dj.id=done.job_id
      WHERE done.account_id=a.account_id AND done.component='installations'
        AND done.attempted_at IS NOT NULL AND done.next_check_at>statement_timestamp() AND dj.status IN ('succeeded','dead')))
 AND (j.id IS NULL OR j.status IN ('succeeded','dead'))
 AND (s.next_check_at IS NULL OR s.next_check_at<=statement_timestamp()
     OR (part.component<>'installations' AND src.installation_id IS NOT NULL
         AND NOT EXISTS(SELECT FROM platform_manager.account_profiles p WHERE p.account_id=a.account_id AND p.classification='Запрет работы')
         AND (s.seed_installation_id,s.seed_generation,s.seed_authorization_sequence)
                IS DISTINCT FROM (src.installation_id,src.generation,src.authorization_sequence)))
 ORDER BY s.next_check_at NULLS FIRST,a.account_id,part.component LIMIT $1`, limit, o.directory != nil)
	if err != nil {
		return err
	}
	type seed struct {
		account uuid.UUID
		part    accountComponent
	}
	seeds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seed, error) {
		var s seed
		err := row.Scan(&s.account, &s.part)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range seeds {
		if err := lockRefreshSlot(ctx, tx, s.account, s.part); err != nil {
			return err
		}
		var scope sourceScope
		if s.part != refreshInstallations {
			scope, err = accountSourceScope(ctx, tx, s.account)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var due bool
		err = tx.QueryRow(ctx, `SELECT next_check_at<=statement_timestamp() OR
 ($3::uuid IS NOT NULL AND (seed_installation_id,seed_generation,seed_authorization_sequence) IS DISTINCT FROM ($3::uuid,$4::bigint,$5::bigint))
 FROM platform_manager.account_refresh_slots WHERE account_id=$1 AND component=$2`, s.account, s.part, nullableSeed(scope), scope.Generation, scope.AuthorizationSequence).Scan(&due)
		if err != nil {
			return err
		}
		if !due {
			continue
		}
		if _, err := joinAccountRefresh(ctx, tx, s.account, s.part, scope); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
