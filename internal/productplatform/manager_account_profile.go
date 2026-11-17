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

type profilePatch struct {
	DisplayName, Note, Classification profileField
	Reason                            string
}

type profileValues struct {
	DisplayName    *string `json:"display_name"`
	Note           *string `json:"note"`
	Classification *string `json:"classification"`
}

func parseProfilePatch(raw []byte) (profilePatch, error) {
	var patch profilePatch
	var name, note, classification json.RawMessage
	if err := readManagerObject(raw, map[string]any{
		"display_name": &name, "note": &note, "classification": &classification, "reason": &patch.Reason,
	}); err != nil {
		return patch, err
	}
	patch.Reason = strings.TrimSpace(patch.Reason)
	if !managerText(patch.Reason, 1, 1000) {
		return patch, errManagerInput
	}
	for _, field := range []struct {
		raw json.RawMessage
		dst *profileField
	}{{name, &patch.DisplayName}, {note, &patch.Note}, {classification, &patch.Classification}} {
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
	err := tx.QueryRow(ctx, `
		SELECT display_name,note,classification,version FROM platform_manager.account_profiles WHERE account_id=$1 FOR UPDATE`,
		cmd.Account).Scan(&before.DisplayName, &before.Note, &before.Classification, &version)
	if err != nil {
		return managerOutcome{}, err
	}
	if version != cmd.Version {
		return managerVersionConflict(cmd, version)
	}
	if patch.DisplayName.Value != nil && !managerText(*patch.DisplayName.Value, 1, 512) ||
		patch.Note.Value != nil && !managerText(*patch.Note.Value, 0, 4000) {
		return managerMutationResponse(http.StatusUnprocessableEntity, 0,
			managerProblem(http.StatusUnprocessableEntity, "validation_failed", cmd.RequestID, 0, 0))
	}
	if value := patch.Classification.Value; value != nil && *value != "Без партнёра" && *value != "Запрет работы" {
		return managerMutationResponse(http.StatusUnprocessableEntity, 0,
			managerProblem(http.StatusUnprocessableEntity, "classification_unavailable", cmd.RequestID, 0, 0))
	}

	after := profileValues{DisplayName: patch.DisplayName.apply(before.DisplayName), Note: patch.Note.apply(before.Note),
		Classification: patch.Classification.apply(before.Classification)}
	err = tx.QueryRow(ctx, `
		UPDATE platform_manager.account_profiles SET display_name=$2,note=$3,classification=$4,version=version+1
		WHERE account_id=$1 AND (display_name,note,classification) IS DISTINCT FROM ($2::text,$3::text,$4::text)
		RETURNING version`, cmd.Account, after.DisplayName, after.Note, after.Classification).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return managerOutcome{}, err
	}
	if err == nil {
		_, err := recordAccountChange(ctx, tx, cmd, accountChange{Account: cmd.Account,
			Kind: accountProfileUpdated, Version: version, Reason: &patch.Reason,
			Details: map[string]profileValues{"before": before, "after": after}})
		if err != nil {
			return managerOutcome{}, err
		}
	}
	detail, version, err := readAccountDetail(ctx, tx, cmd.Account, []accountCapability{})
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, version, detail)
}
