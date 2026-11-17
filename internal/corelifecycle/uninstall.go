package corelifecycle

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

var ErrStaleUninstall = errors.New("uninstall attempt no longer owns this installation")

// BeginUninstall reuses the accepted attempt for a retry while the same
// authorization is current. OAuth retires that attempt before changing tokens.
func (s *Store) BeginUninstall(ctx context.Context, tx pgx.Tx, i Installation) (uuid.UUID, error) {
	if scoped, err := InScope(ctx, tx, i); err != nil || !scoped {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM platform_lifecycle_uninstalls WHERE installation_id=$1 AND NOT key_rotated`,
		i.ID).Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	id = uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_lifecycle_uninstalls(id,installation_id,state) VALUES($1,$2,'pending')`, id, i.ID); err != nil {
		return uuid.Nil, err
	}
	return id, reconcile(ctx, tx, i.ID, uninstallPending)
}

// CheckUninstall is evaluated under the installation lock before capturing
// ownership and again before writing the result of the external operation.
func CheckUninstall(ctx context.Context, tx pgx.Tx, i Installation, attempt uuid.UUID) (bool, error) {
	if i.Status != "uninstalled" {
		return false, ErrStaleUninstall
	}
	if attempt == uuid.Nil {
		scoped, err := InScope(ctx, tx, i)
		if scoped {
			return false, ErrStaleUninstall
		}
		return false, err
	}
	var state string
	err := tx.QueryRow(ctx, `
		SELECT state FROM platform_lifecycle_uninstalls WHERE installation_id=$1 AND id=$2 AND NOT key_rotated`,
		i.ID, attempt).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStaleUninstall
	}
	return state == "confirmed", err
}

func (s *Store) FinishUninstall(ctx context.Context, tx pgx.Tx, i Installation, attempt uuid.UUID) error {
	confirmed, err := CheckUninstall(ctx, tx, i, attempt)
	if err != nil || confirmed {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE platform_lifecycle_uninstalls SET state='confirmed',finished_at=now()
		WHERE id=$1`, attempt); err != nil {
		return err
	}
	if err := resolve(ctx, tx, i.ID, uninstallPending); err != nil {
		return err
	}
	c, err := readCursor(ctx, tx, i.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile(ctx, tx, i.ID, historyUnproven)
	}
	if err != nil || c.closed {
		return err
	}
	c.closed = true
	return s.append(ctx, tx, i, c, wire.Uninstalled, "", "core_uninstall", attempt, "owned_webhooks_removed")
}
