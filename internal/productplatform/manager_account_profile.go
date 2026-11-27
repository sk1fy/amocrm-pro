package productplatform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type profileField struct {
	Present bool
	Value   *string
}

func (f profileField) apply(previous *string) *string {
	if f.Present {
		return f.Value
	}
	return previous
}

type widgetFeedback struct {
	Product string `json:"product_id"`
	Left    bool   `json:"left"`
}

type profilePatch struct {
	Feedback                          *widgetFeedback `json:",omitempty"`
	DisplayName, Note, Classification profileField
	Reason                            string
	PartnerName                       profileField `json:",omitzero"`
}

type profileValues struct {
	DisplayName    *string `json:"display_name"`
	Note           *string `json:"note"`
	Classification *string `json:"classification"`
	PartnerName    *string `json:"partner_name,omitempty"`
}

type profileChangeDetails struct {
	Before                  profileValues `json:"before"`
	After                   profileValues `json:"after"`
	ClassificationConfirmed bool          `json:"classification_confirmed,omitempty"`
}

func parseProfilePatch(raw []byte) (profilePatch, error) {
	var patch profilePatch
	var name, note, classification, partner, feedback json.RawMessage
	if err := readManagerObject(raw, map[string]any{
		"widget_feedback": &feedback, "display_name": &name, "note": &note, "classification": &classification, "partner_name": &partner, "reason": &patch.Reason,
	}); err != nil {
		return patch, err
	}
	patch.Reason = strings.TrimSpace(patch.Reason)
	if !managerText(patch.Reason, 1, 1000) {
		return patch, errManagerInput
	}
	if feedback != nil {
		var value widgetFeedback
		var left *bool
		if err := readManagerObject(feedback, map[string]any{"product_id": &value.Product, "left": &left}); err != nil || left == nil || name != nil || note != nil || classification != nil || partner != nil {
			return patch, errManagerInput
		}
		product, err := managerUUID(value.Product)
		if err != nil {
			return patch, errManagerInput
		}
		value.Product, value.Left = product.String(), *left
		patch.Feedback = &value
	}
	for _, field := range []struct {
		raw json.RawMessage
		dst *profileField
	}{{name, &patch.DisplayName}, {note, &patch.Note}, {classification, &patch.Classification}, {partner, &patch.PartnerName}} {
		if field.raw == nil {
			continue
		}
		field.dst.Present = true
		if err := json.Unmarshal(field.raw, &field.dst.Value); err != nil {
			return patch, errManagerInput
		}
	}
	if patch.DisplayName.Value != nil {
		normalized := strings.TrimSpace(*patch.DisplayName.Value)
		patch.DisplayName.Value = &normalized
	}
	if patch.PartnerName.Value != nil {
		normalized := strings.TrimSpace(*patch.PartnerName.Value)
		patch.PartnerName.Value = &normalized
	}
	return patch, nil
}

func (m managerHTTP) updateAccountProfile(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	key, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, versionErr := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if idErr != nil || keyErr != nil || versionErr != nil || contentType != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	patch, err := parseProfilePatch(raw)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	cmd := managerCommand{ID: env.Command, Key: key, Subject: env.Actor.Subject, RequestID: env.RequestID,
		Account: id, Kind: profileCommandKind, Version: version}
	outcome, err := m.store.mutate(r.Context(), cmd, patch, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return updateAccountProfile(ctx, tx, cmd, patch)
	})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}

func updateAccountProfile(ctx context.Context, tx pgx.Tx, cmd managerCommand, patch profilePatch) (managerOutcome, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_manager.account_profiles(account_id) VALUES($1) ON CONFLICT DO NOTHING`, cmd.Account); err != nil {
		return managerOutcome{}, err
	}
	var before profileValues
	var version int64
	var automaticBlock bool
	err := tx.QueryRow(ctx, `
		SELECT display_name,note,classification,partner_name,version,technical_classification_version IS NOT NULL
		FROM platform_manager.account_profiles WHERE account_id=$1 FOR UPDATE`,
		cmd.Account).Scan(&before.DisplayName, &before.Note, &before.Classification, &before.PartnerName, &version, &automaticBlock)
	if err != nil {
		return managerOutcome{}, err
	}
	if version != cmd.Version {
		return managerVersionConflict(cmd, version)
	}
	if patch.Feedback != nil {
		return updateWidgetFeedback(ctx, tx, cmd, patch)
	}
	if patch.DisplayName.Value != nil && !managerText(*patch.DisplayName.Value, 1, 512) ||
		patch.Note.Value != nil && !managerText(*patch.Note.Value, 0, 4000) {
		return managerMutationResponse(http.StatusUnprocessableEntity, 0,
			managerProblem(http.StatusUnprocessableEntity, "validation_failed", cmd.RequestID, 0, 0))
	}
	if value := patch.Classification.Value; value != nil {
		switch *value {
		case "Свободный клиент", "Запрет работы", "Клиент партнёра", "Наш клиент":
		default:
			return managerMutationResponse(http.StatusUnprocessableEntity, 0,
				managerProblem(http.StatusUnprocessableEntity, "classification_unavailable", cmd.RequestID, 0, 0))
		}
	}

	after := profileValues{DisplayName: patch.DisplayName.apply(before.DisplayName), Note: patch.Note.apply(before.Note),
		Classification: patch.Classification.apply(before.Classification), PartnerName: patch.PartnerName.apply(before.PartnerName)}
	partner := after.Classification != nil && *after.Classification == "Клиент партнёра"
	if !partner && !patch.PartnerName.Present {
		after.PartnerName = nil
	}
	if partner != (after.PartnerName != nil) ||
		after.PartnerName != nil && !managerText(*after.PartnerName, 1, 200) {
		return managerMutationResponse(http.StatusUnprocessableEntity, 0,
			managerProblem(http.StatusUnprocessableEntity, "validation_failed", cmd.RequestID, 0, 0))
	}
	manualOverride := automaticBlock && patch.Classification.Present
	err = tx.QueryRow(ctx, `
		UPDATE platform_manager.account_profiles SET display_name=$2,note=$3,classification=$4,partner_name=$5,version=version+1,
			technical_classification_version=CASE WHEN $6 THEN NULL ELSE technical_classification_version END
		WHERE account_id=$1 AND ($6 OR (display_name,note,classification,partner_name) IS DISTINCT FROM ($2::text,$3::text,$4::text,$5::text))
		RETURNING version`, cmd.Account, after.DisplayName, after.Note, after.Classification, after.PartnerName, manualOverride).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return managerOutcome{}, err
	}
	if err == nil {
		confirmed := manualOverride && after.Classification != nil && *after.Classification == "Запрет работы"
		_, err := recordAccountChange(ctx, tx, cmd, accountChange{Account: cmd.Account,
			Kind: accountProfileUpdated, Version: version, Reason: &patch.Reason,
			Details: profileChangeDetails{Before: before, After: after, ClassificationConfirmed: confirmed}})
		if err != nil {
			return managerOutcome{}, err
		}
		if patch.Classification.Present && after.Classification != nil && *after.Classification == "Запрет работы" {
			if err := skipParkedInstallationDealsForBan(ctx, tx, cmd.Account); err != nil {
				return managerOutcome{}, err
			}
		}
	}
	detail, version, err := readAccountDetail(ctx, tx, cmd.Account, []accountCapability{})
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, version, detail)
}

// The profile version is locked by updateAccountProfile. The flag belongs to
// the product in this account and survives changes to its installation episode.
func updateWidgetFeedback(ctx context.Context, tx pgx.Tx, cmd managerCommand, patch profilePatch) (managerOutcome, error) {
	feedback := patch.Feedback
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform_manager.account_product_directory WHERE account_id=$1 AND product_id=$2)`, cmd.Account, feedback.Product).Scan(&visible); err != nil {
		return managerOutcome{}, err
	}
	if !visible {
		return managerMutationResponse(http.StatusUnprocessableEntity, 0, managerProblem(http.StatusUnprocessableEntity, "validation_failed", cmd.RequestID, 0, 0))
	}
	query := `DELETE FROM platform_manager.widget_feedback WHERE account_id=$1 AND product_id=$2`
	if feedback.Left {
		query = `INSERT INTO platform_manager.widget_feedback(account_id,product_id) VALUES($1,$2) ON CONFLICT DO NOTHING`
	}
	changed, err := tx.Exec(ctx, query, cmd.Account, feedback.Product)
	if err != nil {
		return managerOutcome{}, err
	}
	if changed.RowsAffected() > 0 {
		if _, err = tx.Exec(ctx, `UPDATE platform_manager.account_profiles SET version=version+1 WHERE account_id=$1`, cmd.Account); err != nil {
			return managerOutcome{}, err
		}
		if _, err = recordAccountChange(ctx, tx, cmd, accountChange{Account: cmd.Account, Kind: accountProfileUpdated, Version: cmd.Version + 1, Reason: &patch.Reason, Details: feedback}); err != nil {
			return managerOutcome{}, err
		}
	}
	detail, version, err := readAccountDetail(ctx, tx, cmd.Account, []accountCapability{})
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, version, detail)
}
