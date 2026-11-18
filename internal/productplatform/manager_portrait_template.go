package productplatform

import (
	"context"
	"net/http"
	"reflect"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type portraitTemplateTopic struct {
	ID      uuid.UUID `json:"id"`
	Title   string    `json:"title"`
	Prompts []string  `json:"prompts"`
}

type portraitTemplate struct {
	Topics  []portraitTemplateTopic `json:"topics"`
	Version int64                   `json:"version"`
}

func readPortraitTemplate(ctx context.Context, db portraitQueryer, lock string) (portraitTemplate, error) {
	var template portraitTemplate
	err := db.QueryRow(ctx, `SELECT version,topics FROM platform_manager.portrait_template`+lock).
		Scan(&template.Version, &template.Topics)
	return template, err
}

func (m managerHTTP) getPortraitTemplate(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	if r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	template, err := readPortraitTemplate(r.Context(), m.store.pool, "")
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(template.Version))
	writeLifecycleJSON(w, http.StatusOK, template)
}

func (m managerHTTP) putPortraitTemplate(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	cmd, err := portraitCommand(r, env, portraitTemplateCommandKind, true)
	raw, readErr := readPortraitBody(w, r)
	topics, parseErr := parsePortraitTemplateInput(raw)
	if err != nil || readErr != nil || parseErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	out, err := m.store.mutate(r.Context(), cmd, topics, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		template, err := readPortraitTemplate(ctx, tx, " FOR UPDATE")
		if err != nil {
			return managerOutcome{}, err
		}
		if cmd.Version != template.Version {
			return managerVersionConflict(cmd, template.Version)
		}
		if !reflect.DeepEqual(topics, template.Topics) {
			template.Topics = topics
			err := tx.QueryRow(ctx, `UPDATE platform_manager.portrait_template SET version=version+1,topics=$1
				WHERE singleton RETURNING version`, topics).Scan(&template.Version)
			if err != nil {
				return managerOutcome{}, err
			}
		}
		return managerMutationResponse(http.StatusOK, template.Version, template)
	})
	writePortraitOutcome(w, env.RequestID, out, err)
}

func parsePortraitTemplateInput(raw []byte) ([]portraitTemplateTopic, error) {
	fields, err := portraitObject(raw, "topics")
	if err != nil {
		return nil, err
	}
	topics, err := parsePortraitList(fields["topics"], parsePortraitTemplateTopic, 100)
	if err != nil {
		return nil, err
	}
	seen := make(map[uuid.UUID]bool, len(topics))
	for _, topic := range topics {
		if seen[topic.ID] {
			return nil, errManagerInput
		}
		seen[topic.ID] = true
	}
	return topics, nil
}

func parsePortraitTemplateTopic(raw []byte) (portraitTemplateTopic, error) {
	var topic portraitTemplateTopic
	fields, err := portraitObject(raw, "id", "title", "prompts")
	if err != nil {
		return topic, err
	}
	topic.ID, err = portraitMemberUUID(fields, "id")
	if err != nil || portraitFixedField(topic.ID).Kind != 0 {
		return topic, errManagerInput
	}
	ok, err := portraitMember(fields, "title", &topic.Title, false)
	if err != nil || !ok || !managerText(topic.Title, 1, 300) || strings.TrimSpace(topic.Title) == "" {
		return topic, errManagerInput
	}
	ok, err = portraitMember(fields, "prompts", &topic.Prompts, false)
	if err != nil || !ok || !validPortraitStrings(topic.Prompts, 30, 2000) {
		return topic, errManagerInput
	}
	return topic, nil
}
