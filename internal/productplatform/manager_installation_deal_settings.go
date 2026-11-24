package productplatform

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const installationDealSettingsCommandKind = "installation.deal_settings.set"

type installationDealSettings struct {
	Owners        map[string]string      `json:"owners,omitempty"`
	Version       int64                  `json:"version"`
	Configuration *workflowConfiguration `json:"configuration"`
}

func readInstallationDealSettings(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (installationDealSettings, error) {
	result := installationDealSettings{Version: 1}
	var config workflowConfiguration
	err := db.QueryRow(ctx, `
		SELECT r.revision,s.account_id::text,s.binding_revision,r.metadata_generation,
			r.pipeline_id::text,r.stage_id::text,r.validated_at,r.owners
		FROM platform_company.source_control c
		JOIN platform_company.source_scopes s ON s.id=c.scope_id
		JOIN platform_company.installation_deal_settings_revisions r ON r.scope_id=c.scope_id
		ORDER BY r.revision DESC LIMIT 1`).
		Scan(&config.Revision, &config.Account, &config.Binding, &config.Generation, &config.Pipeline, &config.Stage, &config.ValidatedAt, &result.Owners)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err == nil {
		result.Version = config.Revision + 1
		result.Configuration = &config
	}
	return result, err
}

func saveInstallationDealSettings(ctx context.Context, tx pgx.Tx, cmd managerCommand, input companyWorkflowInput) (managerOutcome, error) {
	scope, account, binding, retry, err := lockCompanyWorkflowSource(ctx, tx)
	if err != nil {
		return managerOutcome{}, err
	}
	current, err := readInstallationDealSettings(ctx, tx)
	if err != nil {
		return managerOutcome{}, err
	}
	if current.Version != cmd.Version {
		return managerVersionConflict(cmd, current.Version)
	}
	if rejected, err := validateCompanyWorkflow(ctx, tx, cmd, input, account, binding, retry); err != nil || rejected.status != 0 {
		return rejected, err
	}
	owners := current.Owners
	if input.Owners != nil {
		owners = *input.Owners
		if len(owners) != 0 {
			employees, err := installationDealEmployees(ctx, tx, input.Generation)
			if err != nil {
				return managerOutcome{}, err
			}
			if employees == nil {
				return managerOutcome{}, &companyMetadataUnavailable{retry}
			}
			active := make(map[string]bool, len(*employees))
			for _, user := range *employees {
				active[user.ID] = true
			}
			for _, id := range owners {
				if !active[id] {
					return managerMutationResponse(422, 0, managerProblem(422, "validation_failed", cmd.RequestID, 0, 0))
				}
			}
		}
	}
	if owners == nil {
		owners = map[string]string{}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO platform_company.installation_deal_settings_revisions
			(scope_id,revision,metadata_generation,pipeline_id,stage_id,command_id,actor_id,owners)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, scope, current.Version, input.Generation, input.Pipeline, input.Stage, cmd.ID, cmd.Subject, owners)
	if err != nil {
		return managerOutcome{}, err
	}
	result, err := readInstallationDealSettings(ctx, tx)
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, result.Version, result)
}

func (m managerHTTP) getInstallationDealSettings(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	if r.URL.RawQuery != "" {
		writeManagerError(w, 400, "validation_failed", env.RequestID, 0)
		return
	}
	result, err := readInstallationDealSettings(r.Context(), m.store.pool)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(result.Version))
	writeLifecycleJSON(w, http.StatusOK, result)
}

func (m managerHTTP) setInstallationDealSettings(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	m.setWorkflow(w, r, env, installationDealSettingsCommandKind, uuid.Nil, saveInstallationDealSettings)
}

func installationDealEmployees(ctx context.Context, tx pgx.Tx, generation uuid.UUID) (*[]workflowStage, error) {
	var employees *[]workflowStage
	err := tx.QueryRow(ctx, `SELECT employees FROM platform_company.generations WHERE id=$1 AND component='metadata'`, generation).Scan(&employees)
	return employees, err
}

func installationDealOwnerKey(classification string) string {
	switch classification {
	case "Наш клиент":
		return "own"
	case "Клиент партнёра":
		return "partner"
	case "Свободный клиент":
		return "direct"
	case "":
		return "unclassified"
	}
	return ""
}
