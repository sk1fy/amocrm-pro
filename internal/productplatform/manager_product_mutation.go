package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func productValidation(cmd managerCommand, code string) (managerOutcome, error) {
	return managerMutationResponse(http.StatusUnprocessableEntity, 0, managerProblem(http.StatusUnprocessableEntity, code, cmd.RequestID, 0, 0))
}

func saveProduct(ctx context.Context, tx pgx.Tx, cmd managerCommand, input productInput, create bool) (managerOutcome, error) {
	id := cmd.Product
	if create {
		id = cmd.Resource
		if _, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle.products(id,code) VALUES($1,$2) ON CONFLICT(code) DO NOTHING`, id, input.Code); err != nil {
			return managerOutcome{}, err
		}
	}
	var code string
	var version *int64
	var current managerProduct
	var displayName, line, availability *string
	var policyVersion *int64
	err := tx.QueryRow(ctx, `SELECT id,code,display_name,line,availability,links,tariff_version,catalog_version,catalog_policy_version,tariff_options,trial_extension_days
		FROM platform_lifecycle.products WHERE id=$1 FOR UPDATE`, id).
		Scan(&current.ID, &code, &displayName, &line,
			&availability, &current.Links, &current.Tariff, &version, &policyVersion, &current.TariffOptions, &current.TrialExtensionDays)
	if err != nil {
		return managerOutcome{}, accountReadError(err)
	}
	current.Code = code
	if displayName != nil {
		current.DisplayName = *displayName
	}
	if line != nil {
		current.Line = *line
	}
	if availability != nil {
		current.Availability = *availability
	}
	if create && version != nil {
		return managerMutationResponse(http.StatusConflict, 0, managerProblem(http.StatusConflict, "product_exists", cmd.RequestID, *version, 0))
	}
	if !create {
		if version == nil {
			return managerOutcome{}, errManagerNotFound
		}
		if *version != cmd.Version {
			return managerVersionConflict(cmd, *version)
		}
	}
	if create {
		if _, err := tx.Exec(ctx, `SELECT platform_manager.adopt_trial_evidence($1)`, id); err != nil {
			return managerOutcome{}, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id FROM platform_lifecycle.integrations WHERE product_id=$1 ORDER BY id`, id)
	if err != nil {
		return managerOutcome{}, err
	}
	current.Integrations, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return managerOutcome{}, err
	}
	var additions []uuid.UUID
	if input.Integrations != nil {
		for _, existing := range current.Integrations {
			if !slices.Contains(*input.Integrations, existing) {
				return productValidation(cmd, "integration_mapping_mismatch")
			}
		}
		for _, ref := range *input.Integrations {
			if !slices.Contains(current.Integrations, ref) {
				additions = append(additions, ref)
			}
		}
	}
	if create && policyVersion == nil {
		var initialRevision int64
		err := tx.QueryRow(ctx, `SELECT version FROM platform_lifecycle.initial_term_policies WHERE product_id=$1`, id).Scan(&initialRevision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return managerOutcome{}, err
		}
		if err == nil {
			policyVersion = &initialRevision
		}
	}
	if policyVersion != nil {
		var rule string
		if err := tx.QueryRow(ctx, `SELECT rule FROM platform_lifecycle.product_trial_revisions WHERE product_id=$1 AND version=$2`, id, *policyVersion).Scan(&rule); err != nil {
			return managerOutcome{}, err
		}
		current.Trial = trialForRule(rule)
	}
	next := current
	if input.Integrations != nil {
		next.Integrations = *input.Integrations
	}
	if input.DisplayName != nil {
		next.DisplayName = *input.DisplayName
	}
	if input.Line != nil {
		next.Line = *input.Line
	}
	if input.Availability != nil {
		next.Availability = *input.Availability
	}
	if input.TrialExtensionDays != nil {
		next.TrialExtensionDays = *input.TrialExtensionDays
	}
	if input.TariffOptions != nil {
		next.TariffOptions = *input.TariffOptions
	}
	if input.TariffSet {
		next.Tariff = input.Tariff
	}
	if input.Trial != nil {
		next.Trial = *input.Trial
	}
	if input.Links != nil {
		next.Links = *input.Links
	}
	rule, valid := productPolicyRule(next.Line, next.Trial)
	if !valid {
		return productValidation(cmd, "trial_policy_incompatible")
	}
	if next.Tariff != nil {
		var verified bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_lifecycle.verified_tariff_versions WHERE product_id=$1 AND version=$2)`, id, *next.Tariff).Scan(&verified); err != nil {
			return managerOutcome{}, err
		}
		if !verified {
			return productValidation(cmd, "tariff_evidence_required")
		}
	}
	if rule == "free" && (create || current.Line != "FREE") {
		// The product row is locked for the duration of this catalog command.
		if _, err := tx.Exec(ctx, `SELECT platform_manager.record_free_category($1,$2)`, id, cmd.Subject); err != nil {
			return managerOutcome{}, err
		}
	}
	var selectedRevision int64
	if policyVersion != nil && sameProductTrial(current.Trial, next.Trial) {
		selectedRevision = *policyVersion
	} else {
		var err error
		selectedRevision, err = selectProductTrial(ctx, tx, cmd, id, rule)
		if errors.Is(err, errManagerInput) {
			return productValidation(cmd, "trial_evidence_required")
		}
		if err != nil {
			return managerOutcome{}, err
		}
	}
	currentRaw, _ := json.Marshal(current)
	nextRaw, _ := json.Marshal(next)
	if version != nil && string(currentRaw) == string(nextRaw) {
		current.Version = *version
		return managerMutationResponse(http.StatusOK, current.Version, current)
	}
	if len(additions) > 0 {
		tag, err := tx.Exec(ctx, `INSERT INTO platform_lifecycle.integrations(id,product_id)
            SELECT id,$2 FROM platform_lifecycle.core_integrations WHERE id=ANY($1::uuid[]) AND active ORDER BY id
            ON CONFLICT DO NOTHING`, additions, id)
		if err != nil {
			return managerOutcome{}, err
		}
		if tag.RowsAffected() != int64(len(additions)) {
			return productValidation(cmd, "integration_mapping_mismatch")
		}
	}
	next.Version = 1
	if version != nil {
		next.Version = *version + 1
	}
	_, err = tx.Exec(ctx, `UPDATE platform_lifecycle.products SET display_name=$2,line=$3,availability=$4,links=$5,
		tariff_version=$6,catalog_version=$7,catalog_policy_version=$8,tariff_options=$9,trial_extension_days=$10 WHERE id=$1`,
		id, next.DisplayName, next.Line, next.Availability, next.Links, next.Tariff, next.Version, selectedRevision, next.TariffOptions, next.TrialExtensionDays)
	if err != nil {
		return managerOutcome{}, err
	}
	if err := applyProductTrial(ctx, tx, id, selectedRevision); err != nil {
		return managerOutcome{}, err
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	return managerMutationResponse(status, next.Version, next)
}

func sameProductTrial(a, b productTrial) bool {
	return a.Kind == b.Kind && (a.Days == nil && b.Days == nil || a.Days != nil && b.Days != nil && *a.Days == *b.Days)
}

func selectProductTrial(ctx context.Context, tx pgx.Tx, cmd managerCommand, id uuid.UUID, rule string) (int64, error) {
	var sourceVersion *int64
	var evidence *string
	if rule != "none" {
		var version int64
		var text string
		err := tx.QueryRow(ctx, `SELECT version,evidence FROM platform_lifecycle.verified_trial_policies
			WHERE product_id=$1 AND rule=$2 ORDER BY version DESC LIMIT 1`, id, rule).Scan(&version, &text)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errManagerInput
		}
		if err != nil {
			return 0, err
		}
		sourceVersion, evidence = &version, &text
	}
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO platform_lifecycle.product_trial_revisions(product_id,version,rule,source_version,evidence,actor_id)
		SELECT $1,COALESCE(max(version),0)+1,$2,$3,$4,$5 FROM platform_lifecycle.product_trial_revisions WHERE product_id=$1
		RETURNING version`, id, rule, sourceVersion, evidence, cmd.Subject).Scan(&revision)
	return revision, err
}

func applyProductTrial(ctx context.Context, tx pgx.Tx, id uuid.UUID, revision int64) error {
	_, err := tx.Exec(ctx, `SELECT platform_manager.apply_catalog_policy($1,$2)`, id, revision)
	return err
}
