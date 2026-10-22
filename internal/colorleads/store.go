package colorleads

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sk1fy/amocrm-pro/internal/widgetauth"
	"time"
)

type Store struct {
	pool  *pgxpool.Pool
	guard Guard
}

func NewStore(pool *pgxpool.Pool, guard Guard) *Store { return &Store{pool: pool, guard: guard} }

// transact consumes the single-use widget JWT in the same transaction as
// product access; wire VerificationMiddleware, never consuming Middleware.
func (s *Store) transact(ctx context.Context, p widgetauth.Principal, f Fence, write bool, run func(pgx.Tx) error) error {
	if s == nil || s.pool == nil || s.guard == nil {
		return ErrNotConfigured
	}
	if p.InstallationID == uuid.Nil || p.IntegrationID == uuid.Nil || p.AccountID <= 0 || p.UserID <= 0 || p.TokenID == "" || !p.TokenRetainUntil.After(time.Now()) {
		return ErrUnauthorized
	}
	if f.Epoch <= 0 || f.Generation <= 0 {
		return ErrRouteRejected
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT i.id FROM installations i JOIN integrations g ON g.id=i.integration_id WHERE i.id=$1 AND i.integration_id=$2 AND i.account_id=$3 AND i.status='active' AND g.status='active' FOR SHARE OF i,g`, p.InstallationID, p.IntegrationID, p.AccountID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if err = s.guard.Admit(ctx, tx, p, f, write); err != nil {
		return err
	}
	used := p.UsedToken()
	tag, err := tx.Exec(ctx, `INSERT INTO used_widget_tokens(integration_id,jti,issuer,account_id,user_id,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(integration_id,jti) DO NOTHING`, used.IntegrationID, used.TokenID, used.Issuer, used.AccountID, used.UserID, used.ExpiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrUnauthorized
	}
	if err = run(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) ReadSettings(ctx context.Context, p widgetauth.Principal, f Fence) (SettingsDocument, error) {
	out := SettingsDocument{Settings: json.RawMessage(`{}`)}
	err := s.transact(ctx, p, f, false, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT settings,settings_revision FROM color_leads_documents WHERE installation_id=$1 AND installation_generation=$2`, p.InstallationID, f.Generation).Scan(&out.Settings, &out.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}
func (s *Store) WriteSettings(ctx context.Context, p widgetauth.Principal, f Fence, raw json.RawMessage, expected int64) (SettingsDocument, error) {
	if ValidateSettings(raw) != nil || expected < 0 {
		return SettingsDocument{}, ErrInvalid
	}
	out := SettingsDocument{Settings: raw}
	err := s.transact(ctx, p, f, true, func(tx pgx.Tx) error {
		if err := ensureDocument(ctx, tx, p, f); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE color_leads_documents SET settings=$3,settings_revision=settings_revision+1,updated_at=now() WHERE installation_id=$1 AND installation_generation=$2 AND settings_revision=$4 RETURNING settings_revision`, p.InstallationID, f.Generation, raw, expected).Scan(&out.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRevisionConflict
		}
		return err
	})
	return out, err
}
func ensureDocument(ctx context.Context, tx pgx.Tx, p widgetauth.Principal, f Fence) error {
	_, err := tx.Exec(ctx, `INSERT INTO color_leads_documents(installation_id,installation_generation) VALUES($1,$2) ON CONFLICT DO NOTHING`, p.InstallationID, f.Generation)
	return err
}
func (s *Store) ReadColors(ctx context.Context, p widgetauth.Principal, f Fence, ids []int64) (ColorsDocument, error) {
	if len(ids) == 0 || len(ids) > MaxColors {
		return ColorsDocument{}, ErrInvalid
	}
	wanted := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 {
			return ColorsDocument{}, ErrInvalid
		}
		wanted[id] = true
	}
	out := ColorsDocument{Colors: []Color{}}
	err := s.transact(ctx, p, f, false, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT colors,colors_revision FROM color_leads_documents WHERE installation_id=$1 AND installation_generation=$2`, p.InstallationID, f.Generation).Scan(&raw, &out.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var rows []Color
		if err = json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		for _, row := range rows {
			if wanted[row.LeadID] {
				out.Colors = append(out.Colors, row)
			}
		}
		return nil
	})
	return out, err
}

// WriteColors replaces the bounded snapshot using optimistic concurrency. It
// deliberately does not emulate SSD's ambiguous create/update key difference.
func (s *Store) WriteColors(ctx context.Context, p widgetauth.Principal, f Fence, colors []Color, expected int64) (ColorsDocument, error) {
	if ValidateColors(colors) != nil || expected < 0 {
		return ColorsDocument{}, ErrInvalid
	}
	if colors == nil {
		colors = []Color{}
	}
	raw, _ := json.Marshal(colors)
	out := ColorsDocument{Colors: colors}
	err := s.transact(ctx, p, f, true, func(tx pgx.Tx) error {
		if err := ensureDocument(ctx, tx, p, f); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE color_leads_documents SET colors=$3,colors_revision=colors_revision+1,updated_at=now() WHERE installation_id=$1 AND installation_generation=$2 AND colors_revision=$4 RETURNING colors_revision`, p.InstallationID, f.Generation, raw, expected).Scan(&out.Revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRevisionConflict
		}
		return err
	})
	return out, err
}
