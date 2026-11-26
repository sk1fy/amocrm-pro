package productplatform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
	"github.com/sk1fy/amocrm-pro/internal/productplatform/queue"
)

const drainLimit = 24
const DrainKind queue.Kind = "lifecycle.drain"

var ErrEventConflict = errors.New("Platform lifecycle event conflicts with the inbox")
var ErrEventScope = errors.New("Platform lifecycle event is outside the pilot scope")

type LifecycleStore struct {
	pool *pgxpool.Pool
}

func NewLifecycleStore(pool *pgxpool.Pool) *LifecycleStore {
	return &LifecycleStore{pool: pool}
}

type stream struct {
	id                                          uuid.UUID
	sequence, generation, authorizationSequence int64
	authorization                               wire.Authorization
	last                                        *wire.Kind
}

func (s *LifecycleStore) Record(ctx context.Context, raw []byte) (wire.Receipt, error) {
	e, err := wire.DecodeEvent(raw)
	if err != nil {
		return wire.Receipt{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return wire.Receipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := mappedProduct(ctx, tx, e); errors.Is(err, pgx.ErrNoRows) {
		if err := note(ctx, tx, e, "scope_unmapped"); err != nil {
			return wire.Receipt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return wire.Receipt{}, err
		}
		return wire.Receipt{}, ErrEventScope
	} else if err != nil {
		return wire.Receipt{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_lifecycle.cursors(installation_id) VALUES($1) ON CONFLICT DO NOTHING`, e.Installation); err != nil {
		return wire.Receipt{}, err
	}
	c, err := lockStream(ctx, tx, e.Installation)
	if err != nil {
		return wire.Receipt{}, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.inbox(event_id,installation_id,sequence,body)
		VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.ID, e.Installation, e.Sequence, raw)
	if err != nil {
		return wire.Receipt{}, err
	}
	if tag.RowsAffected() == 0 {
		var existing []byte
		err := tx.QueryRow(ctx, `SELECT body FROM platform_lifecycle.inbox WHERE event_id=$1`, e.ID).Scan(&existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return wire.Receipt{}, err
		}
		if !bytes.Equal(raw, existing) {
			if err := note(ctx, tx, e, "event_identity_conflict"); err != nil {
				return wire.Receipt{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return wire.Receipt{}, err
			}
			return wire.Receipt{}, ErrEventConflict
		}
	}

	if e.Sequence > c.sequence+1 {
		if err := note(ctx, tx, e, "sequence_gap"); err != nil {
			return wire.Receipt{}, err
		}
	}
	if err := c.drain(ctx, tx); err != nil {
		return wire.Receipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return wire.Receipt{}, err
	}
	return wire.Receipt{EventID: e.ID, State: "recorded"}, nil
}

func lockStream(ctx context.Context, tx pgx.Tx, id uuid.UUID) (stream, error) {
	c := stream{id: id}
	err := tx.QueryRow(ctx, `
		SELECT sequence,generation,authorization_sequence,effective_authorization,last_lifecycle_event
		FROM platform_lifecycle.cursors WHERE installation_id=$1 FOR UPDATE`, id).
		Scan(&c.sequence, &c.generation, &c.authorizationSequence, &c.authorization, &c.last)
	return c, err
}

func (s *LifecycleStore) DrainJob(ctx context.Context, job queue.Job) error {
	var payload struct {
		InstallationID uuid.UUID `json:"installation_id"`
	}
	if json.Unmarshal(job.Payload, &payload) != nil || payload.InstallationID == uuid.Nil {
		return errors.New("invalid lifecycle continuation job")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := lockStream(ctx, tx, payload.InstallationID)
	if err != nil {
		return err
	}
	if err := c.drain(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *stream) drain(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT body FROM platform_lifecycle.inbox
		WHERE installation_id=$1 AND sequence>$2 ORDER BY sequence LIMIT $3`, c.id, c.sequence, drainLimit+1)
	if err != nil {
		return err
	}
	var pending []wire.Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		e, err := wire.DecodeEvent(raw)
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index, e := range pending {
		if e.Sequence != c.sequence+1 {
			break
		}
		if index == drainLimit {
			if err := note(ctx, tx, e, "drain_pending"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT platform_lifecycle.enqueue_drain($1,$2,$3)`, e.Installation, e.Sequence, e.ID)
			return err
		}
		reason, err := c.apply(ctx, tx, e)
		if err != nil {
			return err
		}
		if reason != "" {
			return note(ctx, tx, e, reason)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE platform_lifecycle.reconciliation SET resolved_at=now()
			WHERE installation_id=$1 AND sequence=$2 AND event_id=$3 AND reason<>'event_identity_conflict'
			  AND resolved_at IS NULL`, c.id, e.Sequence, e.ID); err != nil {
			return err
		}
	}
	return nil
}

func mappedProduct(ctx context.Context, tx pgx.Tx, e wire.Event) (uuid.UUID, error) {
	account, _ := wire.AccountID(e.Account)
	var product uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT i.product_id FROM platform_lifecycle.integrations i
		JOIN platform_lifecycle.products p ON p.id=i.product_id
		WHERE i.id=$1 AND (p.catalog_version IS NOT NULL OR EXISTS(SELECT FROM platform_lifecycle.scope s WHERE s.integration_id=i.id AND s.amo_account_id=$2))`, e.Integration, account).Scan(&product)
	return product, err
}

func (c *stream) apply(ctx context.Context, tx pgx.Tx, e wire.Event) (string, error) {
	account, _ := wire.AccountID(e.Account)
	product, err := mappedProduct(ctx, tx, e)
	if errors.Is(err, pgx.ErrNoRows) {
		return "scope_unmapped", nil
	}
	if err != nil {
		return "", err
	}
	if !c.accepts(e) {
		return "invalid_transition", nil
	}

	var accountID uuid.UUID
	if c.generation == 0 {
		accountID, err = bindPair(ctx, tx, account, product, e)
		if err != nil {
			return "", err
		}
		if accountID == uuid.Nil {
			return "account_product_conflict", nil
		}
		if err := grantInitialTerm(ctx, tx, accountID, product, e); err != nil {
			return "", err
		}
	}
	var sameScope bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM platform_lifecycle.account_products p JOIN platform_lifecycle.accounts a ON a.id=p.account_id
		WHERE p.installation_id=$1 AND p.integration_id=$2 AND p.product_id=$3 AND a.amo_account_id=$4)`,
		c.id, e.Integration, product, account).Scan(&sameScope)
	if err != nil {
		return "", err
	}
	if !sameScope {
		return "installation_scope_conflict", nil
	}

	var episode uuid.UUID
	switch e.Kind {
	case wire.Installed, wire.Reinstalled:
		episode = uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO platform_lifecycle.episodes(id,installation_id,generation,opened_at,installed_by)
			VALUES($1,$2,$3,$4,NULLIF($5,'')::bigint)`, episode, c.id, e.Generation, e.OccurredAt, e.InstalledBy); err != nil {
			return "", err
		}
	case wire.Uninstalled:
		if err := tx.QueryRow(ctx, `
			UPDATE platform_lifecycle.episodes SET closed_at=$3
			WHERE installation_id=$1 AND generation=$2 AND closed_at IS NULL RETURNING id`,
			c.id, e.Generation, e.OccurredAt).Scan(&episode); err != nil {
			return "", err
		}
		if e.Generation == 1 {
			if err := skipRemovedInstallationDealWaitingForTariff(ctx, tx, episode); err != nil {
				return "", err
			}
		}
	default:
		if err := tx.QueryRow(ctx, `
			SELECT id FROM platform_lifecycle.episodes WHERE installation_id=$1 AND generation=$2`,
			c.id, e.Generation).Scan(&episode); err != nil {
			return "", err
		}
	}

	c.generation, c.sequence = e.Generation, e.Sequence
	switch e.Kind {
	case wire.AuthorizationChanged:
		c.authorization, c.authorizationSequence = e.Authorization, e.Sequence
	case wire.Uninstalled:
		c.authorization, c.last = wire.Disabled, &e.Kind
	default:
		c.authorization, c.authorizationSequence, c.last = wire.Authorized, e.Sequence, &e.Kind
	}
	if _, err := tx.Exec(ctx, `
		UPDATE platform_lifecycle.cursors SET sequence=$2,generation=$3,authorization_sequence=$4,
			effective_authorization=$5,last_lifecycle_event=$6 WHERE installation_id=$1`,
		c.id, c.sequence, c.generation, c.authorizationSequence, c.authorization, c.last); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.history(event_id,episode_id,kind,occurred_at)
		VALUES($1,$2,$3,$4)`, e.ID, episode, e.Kind, e.OccurredAt)
	return "", err
}

func (c stream) accepts(e wire.Event) bool {
	if c.generation == 0 {
		return e.Kind == wire.Installed && e.Generation == 1 && e.Sequence == 1
	}
	if c.last != nil && *c.last == wire.Uninstalled {
		return e.Kind == wire.Reinstalled && e.Generation == c.generation+1
	}
	return e.Generation == c.generation &&
		(e.Kind == wire.Reauthorized || e.Kind == wire.Uninstalled || e.Kind == wire.AuthorizationChanged)
}

func bindPair(ctx context.Context, tx pgx.Tx, account int64, product uuid.UUID, e wire.Event) (uuid.UUID, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle.accounts(id,amo_account_id)
		VALUES($1,$2) ON CONFLICT DO NOTHING`, uuid.New(), account); err != nil {
		return uuid.Nil, err
	}
	var accountID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM platform_lifecycle.accounts WHERE amo_account_id=$1`, account).Scan(&accountID); err != nil {
		return uuid.Nil, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.account_products(account_id,product_id,installation_id,integration_id)
		VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, accountID, product, e.Installation, e.Integration)
	if err != nil || tag.RowsAffected() == 0 {
		return uuid.Nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform_lifecycle.scope(integration_id,amo_account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, e.Integration, account)
	return accountID, err
}

func note(ctx context.Context, tx pgx.Tx, e wire.Event, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle.reconciliation(installation_id,sequence,event_id,reason)
		VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.Installation, e.Sequence, e.ID, reason)
	return err
}

const lifecycleTimeout = 5 * time.Second
