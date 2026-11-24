// Package corelifecycle journals confirmed Core installation changes.
package corelifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

const JobType = "platform.lifecycle_delivery"

const (
	historyUnproven  = "history_unproven"
	uninstallPending = "uninstall_pending"
)

type Store struct {
	pool *pgxpool.Pool
	jobs *jobs.Store
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, jobs: jobs.NewStore(pool)}
}

type Installation struct {
	InstalledBy       int64
	ID                uuid.UUID
	Integration       uuid.UUID
	Account           int64
	Status            string
	IntegrationStatus string
}

func EffectiveAuthorization(integration, installation string) wire.Authorization {
	if integration != "active" || installation == "uninstalled" || installation == "disabled" {
		return wire.Disabled
	}
	switch installation {
	case "active":
		return wire.Authorized
	case "reauth_required":
		return wire.ReauthRequired
	default:
		return wire.Unknown
	}
}

// LockInstallation follows Core's integration, installation, credential lock order.
func LockInstallation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Installation, error) {
	i := Installation{ID: id}
	if err := tx.QueryRow(ctx, `
		SELECT n.id,n.status FROM integrations n JOIN installations i ON i.integration_id=n.id
		WHERE i.id=$1 FOR SHARE OF n`, id).Scan(&i.Integration, &i.IntegrationStatus); err != nil {
		return i, err
	}
	err := tx.QueryRow(ctx, `SELECT account_id,status FROM installations WHERE id=$1 FOR UPDATE`, id).Scan(&i.Account, &i.Status)
	return i, err
}

func InScope(ctx context.Context, tx pgx.Tx, i Installation) (bool, error) {
	var scoped bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_lifecycle_scope WHERE integration_id=$1 AND account_id=$2)`,
		i.Integration, i.Account).Scan(&scoped)
	return scoped, err
}

type cursor struct {
	generation, sequence, authorizationSequence int64
	closed                                      bool
}

func readCursor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (cursor, error) {
	var c cursor
	err := tx.QueryRow(ctx, `
		SELECT generation,sequence,authorization_sequence,closed FROM platform_lifecycle_cursors
		WHERE installation_id=$1`, id).Scan(&c.generation, &c.sequence, &c.authorizationSequence, &c.closed)
	return c, err
}

// enrollAccount keeps a catalog product unambiguous when an account installs two
// integration UUIDs for that product concurrently. Existing manual scopes survive.
func enrollAccount(ctx context.Context, tx pgx.Tx, integration uuid.UUID, account int64) error {
	var code string
	err := tx.QueryRow(ctx, `SELECT product_code FROM platform_lifecycle_integrations WHERE integration_id=$1 AND auto_enroll`, integration).Scan(&code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,$2))`, code, account); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform_lifecycle_scope(integration_id,account_id)
        SELECT $1::uuid,$2::bigint WHERE NOT EXISTS(
            SELECT FROM platform_lifecycle_scope s JOIN platform_lifecycle_integrations m USING(integration_id)
            WHERE m.product_code=$3 AND s.account_id=$2 AND s.integration_id<>$1)
        ON CONFLICT DO NOTHING`, integration, account, code)
	return err
}

// RecordOAuth runs under the installation row lock taken by the OAuth upsert.
// Its result tells OAuth to replace the destination key fenced to an old attempt.
func (s *Store) RecordOAuth(ctx context.Context, tx pgx.Tx, i Installation, inserted bool, evidence uuid.UUID) (bool, error) {
	if err := enrollAccount(ctx, tx, i.Integration, i.Account); err != nil {
		return false, err
	}
	if inScope, err := InScope(ctx, tx, i); err != nil || !inScope {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE platform_lifecycle_uninstalls
		SET key_rotated=true, state=CASE WHEN state='pending' THEN 'superseded' ELSE state END,
			finished_at=COALESCE(finished_at, now())
		WHERE installation_id=$1 AND NOT key_rotated`, i.ID)
	if err != nil {
		return false, err
	}
	rotate := tag.RowsAffected() > 0
	if rotate {
		if err := resolve(ctx, tx, i.ID, uninstallPending); err != nil {
			return false, err
		}
	}
	if inserted {
		return rotate, s.append(ctx, tx, i, cursor{generation: 1}, wire.Installed, "", "core_oauth", evidence, "oauth_insert")
	}

	c, err := readCursor(ctx, tx, i.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return rotate, reconcile(ctx, tx, i.ID, historyUnproven)
	}
	if err != nil {
		return false, err
	}
	kind := wire.Reauthorized
	if c.closed {
		c.generation++
		c.closed = false
		kind = wire.Reinstalled
	}
	return rotate, s.append(ctx, tx, i, c, kind, "", "core_oauth", evidence, "oauth_upsert")
}

func (s *Store) RecordAuthorization(ctx context.Context, tx pgx.Tx, i Installation, previous, next wire.Authorization, reason string, evidence uuid.UUID) error {
	if previous == next {
		return nil
	}
	if inScope, err := InScope(ctx, tx, i); err != nil || !inScope {
		return err
	}
	c, err := readCursor(ctx, tx, i.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile(ctx, tx, i.ID, historyUnproven)
	}
	if err != nil || c.closed {
		return err
	}
	return s.append(ctx, tx, i, c, wire.AuthorizationChanged, next, "core_policy", evidence, reason)
}

// RecordIntegrationAuthorization runs while the operator holds the integration
// lock. All other lifecycle producers take that lock before the installation.
func (s *Store) RecordIntegrationAuthorization(ctx context.Context, tx pgx.Tx, id uuid.UUID, previous, next string) error {
	if previous == next {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT i.id,i.account_id,i.status FROM installations i
		JOIN platform_lifecycle_scope p ON p.integration_id=i.integration_id AND p.account_id=i.account_id
		WHERE i.integration_id=$1 ORDER BY i.id FOR UPDATE OF i`, id)
	if err != nil {
		return err
	}
	var affected []Installation
	for rows.Next() {
		i := Installation{Integration: id}
		if err := rows.Scan(&i.ID, &i.Account, &i.Status); err != nil {
			rows.Close()
			return err
		}
		affected = append(affected, i)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, i := range affected {
		if err := s.RecordAuthorization(ctx, tx, i, EffectiveAuthorization(previous, i.Status), EffectiveAuthorization(next, i.Status), "integration_"+next, uuid.New()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) append(ctx context.Context, tx pgx.Tx, i Installation, c cursor, kind wire.Kind, auth wire.Authorization, source string, evidence uuid.UUID, reason string) error {
	return s.appendAt(ctx, tx, i, c, kind, auth, source, evidence, reason, time.Now().UTC())
}

func (s *Store) appendAt(ctx context.Context, tx pgx.Tx, i Installation, c cursor, kind wire.Kind, auth wire.Authorization, source string, evidence uuid.UUID, reason string, occurred time.Time) error {
	c.sequence++
	if kind != wire.Uninstalled {
		c.authorizationSequence = c.sequence
	}
	event := wire.Event{
		Version: 1, Account: strconv.FormatInt(i.Account, 10), Integration: i.Integration,
		Installation: i.ID, ID: uuid.New(), Kind: kind, OccurredAt: occurred.UTC(),
		Generation: c.generation, Sequence: c.sequence, Authorization: auth,
		Evidence: wire.Evidence{Source: source, ID: evidence},
	}
	if (kind == wire.Installed || kind == wire.Reinstalled) && i.InstalledBy > 0 {
		event.InstalledBy = strconv.FormatInt(i.InstalledBy, 10)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle_cursors(installation_id,generation,sequence,authorization_sequence,closed)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(installation_id) DO UPDATE SET generation=excluded.generation, sequence=excluded.sequence,
			authorization_sequence=excluded.authorization_sequence, closed=excluded.closed`,
		i.ID, c.generation, c.sequence, c.authorizationSequence, c.closed); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle_ledger(event_id,installation_id,sequence,evidence_id,reason,body)
		VALUES($1,$2,$3,$4,$5,$6)`, event.ID, i.ID, c.sequence, evidence, reason, body); err != nil {
		return err
	}
	return s.enqueue(ctx, tx, i.ID, event.ID)
}

func reconcile(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle_reconciliation(installation_id,reason) VALUES($1,$2)
		ON CONFLICT(installation_id,reason) DO UPDATE SET observed_at=now(),resolved_at=NULL`, id, reason)
	return err
}

func resolve(ctx context.Context, tx pgx.Tx, id uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE platform_lifecycle_reconciliation SET resolved_at=now()
		WHERE installation_id=$1 AND reason=$2 AND resolved_at IS NULL`, id, reason)
	return err
}

func (s *Store) enqueue(ctx context.Context, tx pgx.Tx, installation, event uuid.UUID) error {
	_, err := s.jobs.EnqueueTx(ctx, tx, jobs.EnqueueParams{
		InstallationID: &installation, Type: JobType, ResourceType: "platform_lifecycle",
		ResourceID: event.String(), Payload: map[string]any{"event_id": event}, MaxAttempts: 8,
	})
	return err
}
