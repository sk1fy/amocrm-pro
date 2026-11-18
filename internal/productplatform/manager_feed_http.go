package productplatform

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
)

func (m managerHTTP) feed(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	m.events(w, r, env, "feed")
}

func (m managerHTTP) accountHistory(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	m.events(w, r, env, "history")
}

func (m managerHTTP) events(w http.ResponseWriter, r *http.Request, env managerEnvelope, route string) {
	f, raw, err := parseEventFilters(r.URL.RawQuery, m.options.zone, route)
	if route == "history" && err == nil {
		var id uuid.UUID
		id, err = managerUUID(r.PathValue("id"))
		f.Account = &id
	}
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	var after accountSeek
	hash := f.hash()
	if raw != "" {
		after, err = m.options.parseEventCursor(raw, env.Actor, f)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.listEvents(r.Context(), env.Actor, f, after)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	if next.Snapshot != uuid.Nil {
		cursor := m.options.eventCursor(next, env.Actor, f.Route, hash)
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func parseEventFilters(raw string, zone *time.Location, route string) (eventFilters, string, error) {
	f := eventFilters{Route: route, Zone: zone.String(), Limit: 25}
	query, err := boundedAccountQuery(raw)
	if err != nil {
		return f, "", errManagerInput
	}
	kinds := query["kind"]
	if len(kinds) > 30 {
		return f, "", errManagerInput
	}
	for _, kind := range kinds {
		if _, ok := eventSummaries[kind]; !ok {
			return f, "", errManagerInput
		}
	}
	slices.Sort(kinds)
	f.Kinds = slices.Compact(kinds)
	delete(query, "kind")
	var cursor string
	for key, values := range query {
		if len(values) != 1 || !managerText(values[0], 1, 200) {
			return f, "", errManagerInput
		}
		value := values[0]
		switch key {
		case "cursor":
			cursor = value
		case "limit":
			f.Limit, err = accountInteger(value, 1, 100)
		case "account_id", "product_id":
			if key == "account_id" && route == "history" {
				return f, "", errManagerInput
			}
			var id uuid.UUID
			id, err = managerUUID(value)
			if key == "account_id" {
				f.Account = &id
			} else {
				f.Product = &id
			}
		case "from", "to":
			var day time.Time
			day, err = time.ParseInLocation(time.DateOnly, value, zone)
			if err != nil || day.Year() == 0 || day.Format(time.DateOnly) != value {
				return f, "", errManagerInput
			}
			if key == "from" {
				at := day.UTC()
				f.From, f.start = value, &at
			} else {
				before := day.AddDate(0, 0, 1).UTC()
				f.To, f.before = value, &before
			}
		default:
			return f, "", errManagerInput
		}
		if err != nil {
			return f, "", errManagerInput
		}
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return f, "", errManagerInput
	}
	return f, cursor, nil
}

func (o ManagerOptions) eventCursor(after accountSeek, actor managerActor, route string, hash [32]byte) string {
	var body [20]byte
	copy(body[:16], after.Snapshot[:])
	binary.BigEndian.PutUint32(body[16:], uint32(after.Ordinal))
	return base64.RawURLEncoding.EncodeToString(append(body[:], o.routeCursorMAC("platform-events-v1/"+route, body[:], actor, hash)...))
}

func (o ManagerOptions) parseEventCursor(raw string, actor managerActor, f eventFilters) (accountSeek, error) {
	body, err := managerBase64(raw)
	if err != nil || len(body) != 52 || !hmac.Equal(body[20:], o.routeCursorMAC("platform-events-v1/"+f.Route, body[:20], actor, f.hash())) {
		return accountSeek{}, errManagerCursor
	}
	after := accountSeek{Snapshot: uuid.UUID(body[:16]), Ordinal: int(binary.BigEndian.Uint32(body[16:20]))}
	if after.Snapshot == uuid.Nil || after.Ordinal < 1 || after.Ordinal >= accountSnapshotRows {
		return accountSeek{}, errManagerCursor
	}
	return after, nil
}
