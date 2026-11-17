package productplatform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type accountCapability struct {
	Action         string   `json:"action"`
	Allowed        bool     `json:"allowed"`
	ReasonCodes    []string `json:"reason_codes"`
	RequiredGrants []string `json:"required_grants"`
}

func (m managerHTTP) accounts(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	f, rawCursor, err := parseAccountFilters(r.URL.RawQuery)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	var after accountSeek
	queryHash := f.hash()
	if rawCursor != "" {
		after, err = m.options.parseAccountCursor(rawCursor, env.Actor, queryHash)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.listAccounts(r.Context(), env.Actor, f, after)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	if next.Snapshot != uuid.Nil {
		cursor := m.options.accountCursor(next, env.Actor, queryHash)
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func parseAccountFilters(raw string) (accountFilters, string, error) {
	f := accountFilters{Limit: 25, Sort: "amo_account_id"}
	query, err := boundedAccountQuery(raw)
	if err != nil {
		return f, "", err
	}
	values := query["classification"]
	if len(values) > 30 {
		return f, "", errManagerInput
	}
	for _, value := range values {
		if !managerText(value, 1, 200) {
			return f, "", errManagerInput
		}
	}
	slices.Sort(values)
	f.Classification = slices.Compact(values)
	delete(query, "classification")
	var cursor string
	for key, entries := range query {
		if len(entries) != 1 || !managerText(entries[0], 1, 200) && !(key == "q" && entries[0] == "") {
			return f, "", errManagerInput
		}
		value := entries[0]
		switch key {
		case "q":
			f.Q = strings.TrimSpace(value)
		case "cursor":
			cursor = value
		case "limit":
			f.Limit, err = accountInteger(value, 1, 100)
		case "product_id":
			var id uuid.UUID
			id, err = managerUUID(value)
			f.Product = &id
		case "license_before":
			var when time.Time
			when, err = time.Parse(time.RFC3339Nano, value)
			utc := when.UTC()
			f.LicenseBefore = &utc
		case "active_users_min", "active_users_max":
			var n int
			n, err = accountInteger(value, 0, 2147483647)
			if key == "active_users_min" {
				f.UsersMin = &n
			} else {
				f.UsersMax = &n
			}
		case "sort":
			switch value {
			case "amo_account_id", "current_domain", "display_name", "license_until", "-license_until":
				f.Sort = value
			default:
				err = errManagerInput
			}
		default:
			err = errManagerInput
		}
		if err != nil {
			return f, "", errManagerInput
		}
	}
	if f.UsersMin != nil && f.UsersMax != nil && *f.UsersMin > *f.UsersMax {
		return f, "", errManagerInput
	}
	return f, cursor, nil
}

func boundedAccountQuery(raw string) (url.Values, error) {
	if len(raw) > 4096 {
		return nil, errManagerInput
	}
	return url.ParseQuery(raw)
}

func accountInteger(raw string, minimum, maximum int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum || strconv.Itoa(value) != raw {
		return 0, errManagerInput
	}
	return value, nil
}

func (o ManagerOptions) accountCursor(seek accountSeek, actor managerActor, query [32]byte) string {
	var body [20]byte
	copy(body[:16], seek.Snapshot[:])
	binary.BigEndian.PutUint32(body[16:], uint32(seek.Ordinal))
	return base64.RawURLEncoding.EncodeToString(append(body[:], o.routeCursorMAC("platform-accounts-v1", body[:], actor, query)...))
}

func (o ManagerOptions) parseAccountCursor(raw string, actor managerActor, query [32]byte) (accountSeek, error) {
	body, err := managerBase64(raw)
	if err != nil || len(body) != 52 || !hmac.Equal(body[20:], o.routeCursorMAC("platform-accounts-v1", body[:20], actor, query)) {
		return accountSeek{}, errManagerCursor
	}
	seek := accountSeek{Snapshot: uuid.UUID(body[:16]), Ordinal: int(binary.BigEndian.Uint32(body[16:20]))}
	if seek.Snapshot == uuid.Nil || seek.Ordinal < 1 || seek.Ordinal >= accountSnapshotRows {
		return accountSeek{}, errManagerCursor
	}
	return seek, nil
}

func (m managerHTTP) account(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	refresh := accountCapability{Action: "account.refresh", Allowed: m.accountGateway && env.Actor.Grants["clients.write"],
		RequiredGrants: []string{"clients.write"}, ReasonCodes: []string{}}
	if !env.Actor.Grants["clients.write"] {
		refresh.ReasonCodes = append(refresh.ReasonCodes, "permission_denied")
	}
	if !m.accountGateway {
		refresh.ReasonCodes = append(refresh.ReasonCodes, "source_unavailable")
	}
	profile := accountCapability{Action: profileCommandKind, Allowed: env.Actor.Grants["clients.write"],
		RequiredGrants: []string{"clients.write"}, ReasonCodes: []string{}}
	if !profile.Allowed {
		profile.ReasonCodes = append(profile.ReasonCodes, "permission_denied")
	}
	body, version, err := readAccountDetail(r.Context(), m.store.pool, id, []accountCapability{refresh, profile})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(version))
	writeLifecycleJSON(w, http.StatusOK, body)
}

func (m managerHTTP) crmUsers(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	values, err := boundedAccountQuery(r.URL.RawQuery)
	if err != nil || idErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	var filters = map[string]*bool{"active": nil, "admin": nil}
	for key, entries := range values {
		if _, known := filters[key]; !known || len(entries) != 1 || entries[0] != "true" && entries[0] != "false" {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
		value := entries[0] == "true"
		filters[key] = &value
	}
	body, err := m.store.accountCRMUsers(r.Context(), id, filters["active"], filters["admin"])
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, http.StatusOK, body)
}

func (m managerHTTP) licenseHistory(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	values, err := boundedAccountQuery(r.URL.RawQuery)
	if err != nil || idErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	filters := url.Values{"limit": {"25"}}
	for key, entries := range values {
		if len(entries) != 1 || key != "limit" && key != "from" && key != "to" {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
		value := entries[0]
		if key == "limit" {
			_, err = accountInteger(value, 1, 100)
		} else {
			var day time.Time
			day, err = time.Parse(time.DateOnly, value)
			if day.Year() == 0 {
				err = errManagerInput
			}
		}
		if err != nil {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
		filters.Set(key, value)
	}
	if filters.Get("to") != "" && filters.Get("from") > filters.Get("to") {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	at, err := m.store.accountVisibleAt(r.Context(), id)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	hash := sha256.Sum256([]byte(filters.Encode()))
	writeLifecycleJSON(w, http.StatusOK, struct {
		Items      []json.RawMessage `json:"items"`
		Next       *string           `json:"next_cursor"`
		SnapshotAt time.Time         `json:"snapshot_at"`
		QueryHash  string            `json:"query_hash"`
	}{Items: []json.RawMessage{}, SnapshotAt: at, QueryHash: hex.EncodeToString(hash[:])})
}

func (m managerHTTP) accountWidgets(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idErr := managerUUID(r.PathValue("id"))
	values, err := boundedAccountQuery(r.URL.RawQuery)
	if err != nil || idErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	filters, cursor, err := parseAccountWidgetFilters(values, id, m.options.zone)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	m.writeInstallations(w, r, env, filters, cursor)
}

func parseAccountWidgetFilters(values url.Values, id uuid.UUID, zone *time.Location) (installationFilters, string, error) {
	var lifecycle string
	for key, entries := range values {
		switch key {
		case "state":
			if len(entries) != 1 || entries[0] != "current" && entries[0] != "removed" {
				return installationFilters{}, "", errManagerInput
			}
			lifecycle = entries[0]
		case "cursor", "limit", "product_id":
		default:
			return installationFilters{}, "", errManagerInput
		}
	}
	values.Del("state")
	filters, cursor, err := parseInstallationFilters(values.Encode(), zone)
	filters.Account, filters.Lifecycle = &id, lifecycle
	return filters, cursor, err
}

func (m managerHTTP) refreshAccount(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	account, accountErr := managerUUID(r.PathValue("id"))
	key, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if accountErr != nil || keyErr != nil || contentType != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	var input accountRefreshInput
	if err != nil || readManagerObject(raw, map[string]any{"components": &input.Components, "reason": &input.Reason}) != nil || !input.normalize() {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	body, err := m.store.refreshAccount(r.Context(), accountRefreshCommand{
		ID: env.Command, Key: key, Account: account, Subject: env.Actor.Subject, Input: input,
	}, m.accountGateway)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("Location", managerPath+"operations/"+env.Command.String())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(body)
}

func (m managerHTTP) operation(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	operation, err := m.store.accountOperation(r.Context(), id, env.Actor.Subject, env.RequestID)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, http.StatusOK, operation)
}
