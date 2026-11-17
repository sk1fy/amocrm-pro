package productplatform

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func registerManagerPortrait(mux *http.ServeMux, api managerHTTP) {
	mux.HandleFunc("GET "+managerPath+"accounts/{id}/portrait", managerAuthorized("clients.read", false, api.getPortrait))
	mux.HandleFunc("PATCH "+managerPath+"accounts/{id}/portrait", managerAuthorized("clients.write", true, api.patchPortrait))
	mux.HandleFunc("POST "+managerPath+"accounts/{id}/portrait/audit-dates", managerAuthorized("clients.write", true, api.addPortraitAuditDate))
	mux.HandleFunc("GET "+managerPath+"portrait-template", managerAuthorized("clients.read", false, api.getPortraitTemplate))
	mux.HandleFunc("PUT "+managerPath+"portrait-template", managerAuthorized("configuration.manage", true, api.putPortraitTemplate))
	mux.HandleFunc("GET "+managerPath+"portrait-niches", managerAuthorized("clients.read", false, api.listPortraitNiches))
	mux.HandleFunc("POST "+managerPath+"portrait-niches", managerAuthorized("configuration.manage", true, api.createPortraitNiche))
	mux.HandleFunc("PATCH "+managerPath+"portrait-niches/{id}", managerAuthorized("configuration.manage", true, api.patchPortraitNiche))
	mux.HandleFunc("DELETE "+managerPath+"portrait-niches/{id}", managerAuthorized("configuration.manage", true, api.deletePortraitNiche))
	mux.HandleFunc("POST "+managerPath+"portrait-niches/{id}/merge", managerAuthorized("configuration.manage", true, api.mergePortraitNiche))
	mux.HandleFunc("POST "+managerPath+"accounts/search", managerAuthorized("clients.read", false, api.searchPortraitAccounts))
}

type companyPortrait struct {
	NicheID       *uuid.UUID `json:"niche_id,omitempty"`
	Site          *string    `json:"site,omitempty"`
	Rating        *int       `json:"rating,omitempty"`
	WorkTypes     *[]string  `json:"work_types,omitempty"`
	CustomerTypes *[]string  `json:"customer_types,omitempty"`
}

type portraitRecord struct {
	Company  companyPortrait
	Maturity map[uuid.UUID]bool
	Notes    map[uuid.UUID]string
	Version  int64
}

type portraitMaturity struct {
	ID     uuid.UUID `json:"id"`
	Manual bool      `json:"manual"`
}

type portraitTopicNote struct {
	ID   uuid.UUID `json:"id"`
	Note string    `json:"note"`
}

type portraitAuditDate struct {
	ID   uuid.UUID `json:"id"`
	Date string    `json:"date"`
	Note *string   `json:"note"`
}

type accountPortrait struct {
	Account         uuid.UUID           `json:"account_id"`
	TemplateVersion int64               `json:"template_version"`
	Company         companyPortrait     `json:"company"`
	Maturity        []portraitMaturity  `json:"maturity_signals"`
	Topics          []portraitTopicNote `json:"topics"`
	AuditDates      []portraitAuditDate `json:"audit_dates"`
	Version         int64               `json:"version"`
}

type portraitPatchValue[T any] struct {
	Set   bool
	Value T
}

type portraitCompanyPatch struct {
	NicheID       portraitPatchValue[*uuid.UUID]
	Site          portraitPatchValue[*string]
	Rating        portraitPatchValue[*int]
	WorkTypes     portraitPatchValue[*[]string]
	CustomerTypes portraitPatchValue[*[]string]
}

type portraitMaturityPatch struct {
	ID     uuid.UUID
	Manual *bool
}

type accountPortraitPatch struct {
	Company  portraitCompanyPatch
	Maturity []portraitMaturityPatch
	Topics   []portraitTopicNote
}

type portraitAuditInput struct {
	Date string
	Note *string
}

type portraitQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getAccountPortrait(ctx context.Context, db portraitQueryer, id uuid.UUID) (accountPortrait, error) {
	out := accountPortrait{
		Account: id, Maturity: []portraitMaturity{}, Topics: []portraitTopicNote{}, AuditDates: []portraitAuditDate{},
	}
	var maturity map[uuid.UUID]bool
	var notes map[uuid.UUID]string
	var topics []portraitTemplateTopic
	err := db.QueryRow(ctx, `
		SELECT COALESCE(p.version,1),p.niche_id,p.site,p.rating,p.work_types,p.customer_types,
		       p.maturity,p.topic_notes,t.version,t.topics,
		       COALESCE((SELECT jsonb_agg(jsonb_build_object('id',d.id,'date',d.audit_date,'note',d.note)
		                   ORDER BY d.audit_date DESC) FROM platform_manager.portrait_audit_dates d
		                 WHERE d.account_id=a.account_id),'[]'::jsonb)
		FROM platform_manager.visible_accounts a
		LEFT JOIN platform_manager.account_portraits p USING(account_id)
		CROSS JOIN platform_manager.portrait_template t
		WHERE a.account_id=$1`, id).Scan(&out.Version, &out.Company.NicheID, &out.Company.Site,
		&out.Company.Rating, &out.Company.WorkTypes, &out.Company.CustomerTypes,
		&maturity, &notes, &out.TemplateVersion, &topics, &out.AuditDates)
	if err != nil {
		return out, accountReadError(err)
	}
	for _, id := range portraitManualSignalIDs {
		if value, exists := maturity[id]; exists {
			out.Maturity = append(out.Maturity, portraitMaturity{ID: id, Manual: value})
		}
	}
	for _, topic := range topics {
		if note, exists := notes[topic.ID]; exists {
			out.Topics = append(out.Topics, portraitTopicNote{ID: topic.ID, Note: note})
		}
	}
	return out, nil
}

func lockAccountPortrait(ctx context.Context, tx pgx.Tx, account uuid.UUID) (portraitRecord, error) {
	var p portraitRecord
	if _, err := tx.Exec(ctx, `INSERT INTO platform_manager.account_portraits(account_id) VALUES($1)
		ON CONFLICT(account_id) DO NOTHING`, account); err != nil {
		return p, err
	}
	err := tx.QueryRow(ctx, `SELECT version,niche_id,site,rating,work_types,customer_types,maturity,topic_notes
		FROM platform_manager.account_portraits WHERE account_id=$1 FOR UPDATE`, account).
		Scan(&p.Version, &p.Company.NicheID, &p.Company.Site, &p.Company.Rating, &p.Company.WorkTypes,
			&p.Company.CustomerTypes, &p.Maturity, &p.Notes)
	return p, err
}

func (m managerHTTP) getPortrait(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := getAccountPortrait(r.Context(), m.store.pool, id)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(out.Version))
	writeLifecycleJSON(w, http.StatusOK, out)
}

func (m managerHTTP) patchPortrait(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitAccountCommand(r, env, portraitCommandKind)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := readPortraitBody(w, r)
	input, parseErr := parseAccountPortraitPatch(raw)
	if err != nil || parseErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		return applyAccountPortraitPatch(ctx, tx, cmd, input)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func applyAccountPortraitPatch(ctx context.Context, tx pgx.Tx, cmd managerCommand, input accountPortraitPatch) (managerOutcome, error) {
	var activeTopics map[uuid.UUID]bool
	if len(input.Topics) != 0 {
		template, err := readPortraitTemplate(ctx, tx, " FOR SHARE")
		if err != nil {
			return managerOutcome{}, err
		}
		activeTopics = make(map[uuid.UUID]bool, len(template.Topics))
		for _, topic := range template.Topics {
			activeTopics[topic.ID] = true
		}
	}
	nicheActive := true
	if input.Company.NicheID.Set && input.Company.NicheID.Value != nil {
		err := tx.QueryRow(ctx, `SELECT removed_at IS NULL
			FROM platform_manager.portrait_niches WHERE id=$1 FOR KEY SHARE`,
			*input.Company.NicheID.Value).Scan(&nicheActive)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return managerOutcome{}, err
		}
		nicheActive = err == nil && nicheActive
	}
	p, err := lockAccountPortrait(ctx, tx, cmd.Account)
	if err != nil {
		return managerOutcome{}, err
	}
	if cmd.Version != p.Version {
		return managerVersionConflict(cmd, p.Version)
	}
	for _, note := range input.Topics {
		if !activeTopics[note.ID] {
			return portraitBusinessRule(cmd)
		}
	}
	if !nicheActive {
		return portraitBusinessRule(cmd)
	}
	previous := p
	previous.Maturity, previous.Notes = maps.Clone(p.Maturity), maps.Clone(p.Notes)
	if input.Company.NicheID.Set {
		p.Company.NicheID = input.Company.NicheID.Value
	}
	if input.Company.Site.Set {
		p.Company.Site = input.Company.Site.Value
	}
	if input.Company.Rating.Set {
		p.Company.Rating = input.Company.Rating.Value
	}
	if input.Company.WorkTypes.Set {
		p.Company.WorkTypes = input.Company.WorkTypes.Value
	}
	if input.Company.CustomerTypes.Set {
		p.Company.CustomerTypes = input.Company.CustomerTypes.Value
	}
	changed := !reflect.DeepEqual(previous.Company, p.Company)
	for _, signal := range input.Maturity {
		value, exists := p.Maturity[signal.ID]
		if signal.Manual == nil {
			delete(p.Maturity, signal.ID)
			changed = changed || exists
		} else {
			p.Maturity[signal.ID] = *signal.Manual
			changed = changed || !exists || value != *signal.Manual
		}
	}
	for _, topic := range input.Topics {
		value, exists := p.Notes[topic.ID]
		if topic.Note == "" {
			delete(p.Notes, topic.ID)
			changed = changed || exists
		} else {
			p.Notes[topic.ID] = topic.Note
			changed = changed || !exists || value != topic.Note
		}
	}
	if changed {
		err := tx.QueryRow(ctx, `UPDATE platform_manager.account_portraits SET
			version=version+1,niche_id=$2,site=$3,rating=$4,work_types=$5,customer_types=$6,maturity=$7,topic_notes=$8
			WHERE account_id=$1 RETURNING version`, cmd.Account, p.Company.NicheID, p.Company.Site,
			p.Company.Rating, p.Company.WorkTypes, p.Company.CustomerTypes, p.Maturity, p.Notes).Scan(&p.Version)
		if err != nil {
			return managerOutcome{}, err
		}
		if _, err := recordAccountChange(ctx, tx, cmd, accountChange{
			Account: cmd.Account, Kind: accountPortraitUpdated, Version: p.Version,
			Details: map[string]any{"before": previous, "after": p},
		}); err != nil {
			return managerOutcome{}, err
		}
	}
	return portraitMutationResult(ctx, tx, cmd.Account)
}

func (m managerHTTP) addPortraitAuditDate(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitAccountCommand(r, env, portraitAuditCommandKind)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := readPortraitBody(w, r)
	input, parseErr := parsePortraitAuditDate(raw)
	if err != nil || parseErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		p, err := lockAccountPortrait(ctx, tx, cmd.Account)
		if err != nil {
			return managerOutcome{}, err
		}
		if cmd.Version != p.Version {
			return managerVersionConflict(cmd, p.Version)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO platform_manager.portrait_audit_dates(account_id,audit_date,id,note)
			VALUES($1,$2::text::date,$3,$4) ON CONFLICT(account_id,audit_date) DO NOTHING`,
			cmd.Account, input.Date, cmd.ID, input.Note)
		if err != nil {
			return managerOutcome{}, err
		}
		if tag.RowsAffected() == 0 {
			return managerMutationResponse(http.StatusConflict, 0, managerProblem(
				http.StatusConflict, "state_conflict", cmd.RequestID, p.Version, 0))
		}
		err = tx.QueryRow(ctx, `UPDATE platform_manager.account_portraits SET version=version+1
			WHERE account_id=$1 RETURNING version`, cmd.Account).Scan(&p.Version)
		if err != nil {
			return managerOutcome{}, err
		}
		if _, err := recordAccountChange(ctx, tx, cmd, accountChange{
			Account: cmd.Account, Kind: accountPortraitUpdated, Version: p.Version,
			Details: map[string]any{"audit_date": input.Date, "note": input.Note},
		}); err != nil {
			return managerOutcome{}, err
		}
		return portraitMutationResult(ctx, tx, cmd.Account)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func portraitMutationResult(ctx context.Context, tx pgx.Tx, account uuid.UUID) (managerOutcome, error) {
	out, err := getAccountPortrait(ctx, tx, account)
	if err != nil {
		return managerOutcome{}, err
	}
	return managerMutationResponse(http.StatusOK, out.Version, out)
}

func portraitBusinessRule(cmd managerCommand) (managerOutcome, error) {
	return managerMutationResponse(http.StatusUnprocessableEntity, 0, managerProblem(
		http.StatusUnprocessableEntity, "business_rule_failed", cmd.RequestID, 0, 0))
}

func portraitAccountCommand(r *http.Request, env managerEnvelope, kind string) (managerCommand, error) {
	cmd, err := portraitCommand(r, env, kind, true)
	if err != nil {
		return cmd, err
	}
	cmd.Account, err = managerUUID(r.PathValue("id"))
	return cmd, err
}

func portraitCommand(r *http.Request, env managerEnvelope, kind string, versioned bool) (managerCommand, error) {
	cmd := managerCommand{ID: env.Command, Subject: env.Actor.Subject, RequestID: env.RequestID, Kind: kind}
	if r.URL.RawQuery != "" {
		return cmd, errManagerInput
	}
	var err error
	cmd.Key, err = managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	if err == nil && versioned {
		cmd.Version, err = managerVersion(singleManagerHeader(r.Header, "If-Match"))
	}
	return cmd, err
}

func writePortraitOutcome(w http.ResponseWriter, requestID uuid.UUID, out managerOutcome, err error) {
	if err != nil {
		writeManagerStoreError(w, requestID, err)
		return
	}
	writeManagerOutcome(w, out)
}
