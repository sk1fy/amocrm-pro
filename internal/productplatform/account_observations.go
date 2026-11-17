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
)

type AccountObserver struct {
	pool    *pgxpool.Pool
	gateway platformpb.CoreAccountObservationsClient
}

func NewAccountObserver(pool *pgxpool.Pool, gateway platformpb.CoreAccountObservationsClient) *AccountObserver {
	return &AccountObserver{pool: pool, gateway: gateway}
}

type sourceScope struct {
	AmoAccount                        int64
	Installation, Integration         uuid.UUID
	Generation, AuthorizationSequence int64
}

const validAccountObservationScope = `current AND effective_authorization='authorized' AND generation>0 AND authorization_sequence>0`

type accountScopeReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func accountSourceScope(ctx context.Context, db accountScopeReader, account uuid.UUID) (sourceScope, error) {
	var scope sourceScope
	err := db.QueryRow(ctx, `
		SELECT amo_account_id,installation_id,integration_id,generation,authorization_sequence
		FROM platform_manager.account_memberships
		WHERE account_id=$1 AND `+validAccountObservationScope+`
		ORDER BY installation_id LIMIT 1`, account).
		Scan(&scope.AmoAccount, &scope.Installation, &scope.Integration, &scope.Generation, &scope.AuthorizationSequence)
	return scope, err
}

type accountRefreshPayload struct {
	Account   uuid.UUID        `json:"account_id"`
	Component accountComponent `json:"component"`
}

type accountIdentityObservation struct {
	Domain, Name string
	TimeZone     *string
}

type accountCRMUser struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"is_active"`
	Admin  bool   `json:"is_admin"`
}

type accountObservation struct {
	At       time.Time
	Identity *accountIdentityObservation
	Roster   []accountCRMUser
	Active   int
}

func (o *AccountObserver) RefreshJob(ctx context.Context, job queue.Job) queue.Completion {
	var p accountRefreshPayload
	if json.Unmarshal(job.Payload, &p) != nil || p.Account == uuid.Nil || !p.Component.valid() {
		return queue.Completion{Outcome: queue.Outcome{FailureCode: "invalid_refresh_job"}}
	}
	scope, err := accountSourceScope(ctx, o.pool, p.Account)
	var observation accountObservation
	var outcome queue.Outcome
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		outcome = queue.Outcome{FailureCode: accountSourceChanged}
	case err != nil:
		return queue.Completion{Outcome: queue.Outcome{FailureCode: accountSourceUnavailable, Retry: true}}
	default:
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		observation, outcome = observeAccount(callCtx, o.gateway, scope, p.Component)
		cancel()
	}
	return queue.Completion{Publish: func(ctx context.Context, tx pgx.Tx) (queue.Outcome, error) {
		return publishAccountObservation(ctx, tx, p, scope, observation, outcome)
	}}
}

func observeAccount(ctx context.Context, gateway platformpb.CoreAccountObservationsClient, scope sourceScope,
	part accountComponent) (accountObservation, queue.Outcome) {
	component := platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_LICENSE
	if part == refreshUsers {
		component = platformpb.AccountObservationComponent_ACCOUNT_OBSERVATION_COMPONENT_USERS
	}
	response, err := gateway.ObserveAccount(ctx, &platformpb.ObserveAccountRequest{
		AmoAccountId: scope.AmoAccount, CoreInstallationId: scope.Installation.String(), IntegrationId: scope.Integration.String(),
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
		out.Identity = &accountIdentityObservation{Domain: identity.CurrentDomain, Name: identity.DisplayName, TimeZone: identity.TimeZone}
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
	obs accountObservation, outcome queue.Outcome) (queue.Outcome, error) {
	if scope.Installation != uuid.Nil {
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
			INSERT INTO platform_manager.account_rosters(account_id,roster,active_count,evidence_id,observed_at,fresh_until)
			VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(account_id) DO UPDATE SET roster=EXCLUDED.roster,active_count=EXCLUDED.active_count,
				evidence_id=EXCLUDED.evidence_id,observed_at=EXCLUDED.observed_at,fresh_until=EXCLUDED.fresh_until,failure_code=NULL
			WHERE platform_manager.account_rosters.observed_at IS NULL
			   OR platform_manager.account_rosters.observed_at<=EXCLUDED.observed_at`,
			p.Account, obs.Roster, obs.Active, uuid.New(), obs.At, obs.At.Add(rosterFreshness))
		return outcome, err
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
	for {
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := o.enroll(queryCtx)
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

func (o *AccountObserver) enroll(ctx context.Context) error {
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
	rows, err := tx.Query(ctx, `
		WITH sources AS (
			SELECT DISTINCT ON(account_id) account_id,amo_account_id,installation_id,integration_id,generation,authorization_sequence
			FROM platform_manager.account_memberships WHERE `+validAccountObservationScope+`
			ORDER BY account_id,installation_id
		)
		SELECT a.account_id,part.component,a.amo_account_id,a.installation_id,a.integration_id,a.generation,a.authorization_sequence
		FROM sources a CROSS JOIN (VALUES ('license'),('users')) AS part(component)
		LEFT JOIN platform_manager.account_refresh_slots s ON s.account_id=a.account_id AND s.component=part.component
		LEFT JOIN platform_jobs.jobs j ON j.id=s.job_id
		WHERE (j.id IS NULL OR j.status IN ('succeeded','dead'))
		  AND (s.seed_installation_id,s.seed_generation,s.seed_authorization_sequence)
		      IS DISTINCT FROM (a.installation_id,a.generation,a.authorization_sequence)
		ORDER BY a.account_id,part.component LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type seed struct {
		account uuid.UUID
		part    accountComponent
		scope   sourceScope
	}
	seeds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seed, error) {
		var s seed
		err := row.Scan(&s.account, &s.part, &s.scope.AmoAccount, &s.scope.Installation, &s.scope.Integration,
			&s.scope.Generation, &s.scope.AuthorizationSequence)
		return s, err
	})
	if err != nil {
		return err
	}
	for _, s := range seeds {
		if err := lockRefreshSlot(ctx, tx, s.account, s.part); err != nil {
			return err
		}
		var unchanged bool
		err := tx.QueryRow(ctx, `
			SELECT (seed_installation_id,seed_generation,seed_authorization_sequence) IS NOT DISTINCT FROM ($3::uuid,$4::bigint,$5::bigint)
			FROM platform_manager.account_refresh_slots WHERE account_id=$1 AND component=$2`,
			s.account, s.part, s.scope.Installation, s.scope.Generation, s.scope.AuthorizationSequence).Scan(&unchanged)
		if err != nil {
			return err
		}
		if unchanged {
			continue
		}
		if _, err := joinAccountRefresh(ctx, tx, s.account, s.part, s.scope); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
