// Package services defines the compile-time service catalog and tenant-bound
// capability authorization shared by API admission and worker execution.
package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const LeadStatus = "lead-status"

var ErrNotEnabled = errors.New("service is not enabled")

func Known(code string) bool { return code == LeadStatus }

// JobService deliberately rejects unknown jobs. Infrastructure ping needs no
// product capability; all product jobs must have an explicit catalog entry.
func JobService(jobType string) (string, bool) {
	switch jobType {
	case "widget.ping":
		return "", true
	case "workflow.lead.set_status", "workflow.rule.lead_status.configure", "workflow.lead.status_transition":
		return LeadStatus, true
	default:
		return "", false
	}
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) IsEnabled(ctx context.Context, integrationID, installationID uuid.UUID, code string) (bool, error) {
	if !Known(code) || integrationID == uuid.Nil || installationID == uuid.Nil {
		return false, nil
	}
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM installations AS installation
		JOIN integrations AS integration ON integration.id=installation.integration_id
		JOIN integration_services AS capability ON capability.integration_id=integration.id
		WHERE installation.id=$1 AND integration.id=$2 AND capability.service_code=$3
		  AND installation.status='active' AND integration.status='active' AND capability.enabled
	)`, installationID, integrationID, code).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("read service capability: %w", err)
	}
	return enabled, nil
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// RequireEnabled derives the integration from the trusted installation. When
// lock is true, db must be the transaction enclosing admission or mutation:
// capability revocation and tenant disable then wait until it has completed.
func RequireEnabled(ctx context.Context, db Querier, installationID uuid.UUID, code string, lock bool) error {
	if !Known(code) || installationID == uuid.Nil {
		return ErrNotEnabled
	}
	query := `SELECT 1 FROM installations AS installation
		JOIN integrations AS integration ON integration.id=installation.integration_id
		JOIN integration_services AS capability ON capability.integration_id=integration.id
		WHERE installation.id=$1 AND capability.service_code=$2
		  AND installation.status='active' AND integration.status='active' AND capability.enabled`
	if lock {
		query += ` FOR SHARE OF installation, integration, capability`
	}
	var marker int
	err := db.QueryRow(ctx, query, installationID, code).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotEnabled
	}
	if err != nil {
		return fmt.Errorf("authorize service capability: %w", err)
	}
	return nil
}
