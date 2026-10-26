package crmevents

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// One owner-local read for the returned page, in its repeatable-read snapshot.
// Only the allowlisted fields cross the DB boundary; no note text, recordings,
// catalogs, or per-event Gateway/detail requests are loaded by this path.
func loadCallProjections(ctx context.Context, tx pgx.Tx, principal serviceapi.Principal, events []serviceapi.Event) error {
	ids := []string{}
	byID := map[string]int{}
	for i := range events {
		events[i].CallFacts = projectEmbeddedCall(events[i])
		if events[i].CallFacts != nil && events[i].CallFacts.Source == "" {
			ids = append(ids, events[i].ID)
			byID[events[i].ID] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT l.event_id,o.source,jsonb_build_object('duration',o.payload->'facts'->'duration','phone',o.payload->'facts'->'phone','result',o.payload->'facts'->'result') FROM event_enrichment_links l JOIN event_enrichment_objects o USING (installation_id,object_kind,object_key) WHERE l.installation_id=$1 AND l.event_id=ANY($2::text[]) AND o.object_kind='note' AND o.state='ready' AND o.source<>'event_payload' ORDER BY l.event_id,o.object_key`, principal.InstallationID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source string
		var raw json.RawMessage
		if err := rows.Scan(&id, &source, &raw); err != nil {
			return err
		}
		i, ok := byID[id]
		if ok && events[i].CallFacts.Source == "" {
			events[i].CallFacts = projectCallFacts(events[i].Type, raw, source)
		}
	}
	return rows.Err()
}
