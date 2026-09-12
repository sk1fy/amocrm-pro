package activity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

const panelSelect = `SELECT id,name,employee_ids,display_from,display_to,timezone,enabled,revision,updated_at,view_key_hash IS NOT NULL,view_key_version FROM panels`

func (s *Postgres) ResolveShare(ctx context.Context, hash []byte) (serviceapi.ShareLookup, error) {
	if len(hash) != 32 {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	var lookup serviceapi.ShareLookup
	err := s.pool.QueryRow(ctx, `SELECT installation_id,integration_id,id,enabled,view_key_version,employee_ids FROM panels WHERE view_key_hash=$1`, hash).Scan(&lookup.InstallationID, &lookup.IntegrationID, &lookup.PanelID, &lookup.Enabled, &lookup.ViewKeyVersion, &lookup.EmployeeIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.ShareLookup{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if err != nil {
		return serviceapi.ShareLookup{}, err
	}
	return lookup, nil
}

func (s *Postgres) CreatePanel(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, panel serviceapi.ManagedPanel, hash []byte) (serviceapi.ManagedPanel, error) {
	id, err := uuid.Parse(c.CommandID)
	if err != nil || id == uuid.Nil || panel.ID == uuid.Nil || len(hash) != 32 {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.InvalidArgument, "command_id must be a UUID")
	}
	canonical, _ := json.Marshal(struct {
		Scope         serviceapi.Scope
		Name          string
		EmployeeIDs   []int64
		DisplayWindow serviceapi.DisplayWindow
		Enabled       bool
	}{p.Scope, panel.Name, panel.EmployeeIDs, panel.DisplayWindow, panel.Enabled})
	requestHash := sha256.Sum256(canonical)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `INSERT INTO panel_commands(command_id,installation_id,integration_id,panel_id,kind,request_hash) VALUES($1,$2,$3,$4,'create',$5) ON CONFLICT(command_id) DO NOTHING`, id, p.InstallationID, p.IntegrationID, panel.ID, requestHash[:])
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if tag.RowsAffected() == 0 {
		var existing []byte
		var panelID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT request_hash,panel_id FROM panel_commands WHERE command_id=$1 AND installation_id=$2 AND integration_id=$3 AND kind='create'`, id, p.InstallationID, p.IntegrationID).Scan(&existing, &panelID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
			}
			return serviceapi.ManagedPanel{}, err
		}
		if !bytes.Equal(requestHash[:], existing) {
			return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
		replay, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1 AND installation_id=$2 AND integration_id=$3`, panelID, p.InstallationID, p.IntegrationID))
		if err != nil {
			return serviceapi.ManagedPanel{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
		return replay, nil
	}
	// Serialize quota admission across processes for this installation/integration.
	// Hash collisions only serialize unrelated scopes; they cannot bypass the quota.
	lockHash := sha256.Sum256([]byte("activity-panel-quota:" + p.InstallationID.String() + ":" + p.IntegrationID.String()))
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(lockHash[:8]))); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM panels WHERE installation_id=$1 AND integration_id=$2`, p.InstallationID, p.IntegrationID).Scan(&count); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if count >= serviceapi.MaxPanelsPerInstallation {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.ResourceExhausted, "at most 50 panels per installation")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO panels(id,installation_id,integration_id,name,employee_ids,display_from,display_to,timezone,enabled,revision,view_key_hash,view_key_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$10,1)`, panel.ID, p.InstallationID, p.IntegrationID, panel.Name, panel.EmployeeIDs, panel.DisplayWindow.From, panel.DisplayWindow.To, panel.Timezone, panel.Enabled, hash); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	stored, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1`, panel.ID))
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	stored.ViewKey = panel.ViewKey
	return stored, nil
}

func (s *Postgres) ListPanels(ctx context.Context, scope serviceapi.Scope) ([]serviceapi.ManagedPanel, error) {
	rows, err := s.pool.Query(ctx, panelSelect+` WHERE installation_id=$1 AND integration_id=$2 ORDER BY updated_at DESC, id`, scope.InstallationID, scope.IntegrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panels []serviceapi.ManagedPanel
	for rows.Next() {
		panel, err := scanPanel(rows)
		if err != nil {
			return nil, err
		}
		panels = append(panels, panel)
	}
	return panels, rows.Err()
}

func (s *Postgres) GetPanel(ctx context.Context, scope serviceapi.Scope, id uuid.UUID) (serviceapi.ManagedPanel, error) {
	if id == uuid.Nil {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	panel, err := scanPanel(s.pool.QueryRow(ctx, panelSelect+` WHERE id=$1 AND installation_id=$2 AND integration_id=$3`, id, scope.InstallationID, scope.IntegrationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	return panel, err
}

func (s *Postgres) PatchPanel(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand) (serviceapi.ManagedPanel, error) {
	if c.PanelID == uuid.Nil {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	defer rollback(tx)
	current, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1 AND installation_id=$2 AND integration_id=$3 FOR UPDATE`, c.PanelID, p.InstallationID, p.IntegrationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if current.Revision != c.Revision {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "revision mismatch")
	}
	name, employees, from, to, enabled := current.Name, current.EmployeeIDs, current.DisplayWindow.From, current.DisplayWindow.To, current.Enabled
	if c.HasName {
		name = c.Name
	}
	if c.HasEmployees {
		employees = c.EmployeeIDs
	}
	if c.HasWindow {
		from, to = c.DisplayWindow.From, c.DisplayWindow.To
	}
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	tag, err := tx.Exec(ctx, `UPDATE panels SET name=$1,employee_ids=$2,display_from=$3,display_to=$4,enabled=$5,revision=revision+1,updated_at=now() WHERE id=$6 AND installation_id=$7 AND integration_id=$8 AND revision=$9`, name, employees, from, to, enabled, c.PanelID, p.InstallationID, p.IntegrationID, c.Revision)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if tag.RowsAffected() != 1 {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "revision mismatch")
	}
	updated, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1`, c.PanelID))
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	return updated, nil
}

func (s *Postgres) RotateShareLink(ctx context.Context, p serviceapi.Principal, c serviceapi.PanelCommand, hash []byte, viewKey string) (serviceapi.ManagedPanel, error) {
	id, err := uuid.Parse(c.CommandID)
	if err != nil || id == uuid.Nil || c.PanelID == uuid.Nil || len(hash) != 32 {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.InvalidArgument, "command_id must be a UUID")
	}
	canonical, _ := json.Marshal(struct {
		Scope   serviceapi.Scope
		PanelID uuid.UUID
		Kind    string
	}{p.Scope, c.PanelID, "rotate"})
	requestHash := sha256.Sum256(canonical)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `INSERT INTO panel_commands(command_id,installation_id,integration_id,panel_id,kind,request_hash) VALUES($1,$2,$3,$4,'rotate',$5) ON CONFLICT(command_id) DO NOTHING`, id, p.InstallationID, p.IntegrationID, c.PanelID, requestHash[:])
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if tag.RowsAffected() == 0 {
		var existing []byte
		var panelID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT request_hash,panel_id FROM panel_commands WHERE command_id=$1 AND installation_id=$2 AND integration_id=$3 AND kind='rotate'`, id, p.InstallationID, p.IntegrationID).Scan(&existing, &panelID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
			}
			return serviceapi.ManagedPanel{}, err
		}
		if !bytes.Equal(requestHash[:], existing) || panelID != c.PanelID {
			return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.Conflict, "command_id has different content")
		}
		replay, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1 AND installation_id=$2 AND integration_id=$3`, panelID, p.InstallationID, p.IntegrationID))
		if err != nil {
			return serviceapi.ManagedPanel{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return serviceapi.ManagedPanel{}, err
		}
		return replay, nil
	}
	tag, err = tx.Exec(ctx, `UPDATE panels SET view_key_hash=$1,view_key_version=view_key_version+1,revision=revision+1,updated_at=now() WHERE id=$2 AND installation_id=$3 AND integration_id=$4`, hash, c.PanelID, p.InstallationID, p.IntegrationID)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if tag.RowsAffected() != 1 {
		return serviceapi.ManagedPanel{}, serviceapi.Fail(serviceapi.NotFound, "not found")
	}
	stored, err := scanPanel(tx.QueryRow(ctx, panelSelect+` WHERE id=$1`, c.PanelID))
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	stored.ViewKey = viewKey
	return stored, nil
}

type panelRow interface {
	Scan(dest ...any) error
}

func scanPanel(row panelRow) (serviceapi.ManagedPanel, error) {
	var panel serviceapi.ManagedPanel
	var issued bool
	err := row.Scan(&panel.ID, &panel.Name, &panel.EmployeeIDs, &panel.DisplayWindow.From, &panel.DisplayWindow.To, &panel.Timezone, &panel.Enabled, &panel.Revision, &panel.UpdatedAt, &issued, &panel.ViewKeyVersion)
	if err != nil {
		return serviceapi.ManagedPanel{}, err
	}
	if panel.EmployeeIDs == nil {
		panel.EmployeeIDs = []int64{}
	}
	panel.ShareUrlIssued = issued
	return panel, nil
}
