package productplatform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

type ManagerOptions struct {
	cursorKey []byte
	zone      *time.Location
}

func ParseManagerOptions(key, zone string) (*ManagerOptions, error) {
	if key == "" {
		return nil, nil
	}
	secret, err := managerBase64(key)
	if err != nil || len(secret) != 32 {
		return nil, errors.New("PLATFORM_MANAGER_CURSOR_KEY must be 32 bytes encoded as unpadded base64url")
	}
	if zone == "" {
		zone = "Europe/Moscow"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return nil, errors.New("PLATFORM_MANAGER_TIME_ZONE must be an IANA time zone")
	}
	return &ManagerOptions{cursorKey: secret, zone: loc}, nil
}

func ManagerHandler(pool *pgxpool.Pool, options *ManagerOptions, accountGateway bool) http.Handler {
	if options == nil {
		return nil
	}
	api := managerHTTP{store: managerStore{pool: pool}, options: options, accountGateway: accountGateway}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+managerPath+"installations", managerAuthorized("clients.read", false, api.list))
	mux.HandleFunc("PUT "+managerPath+"installations/{id}/review", managerAuthorized("clients.write", true, api.review))
	mux.HandleFunc("POST "+managerPath+"installations/{id}/deal-retry", managerAuthorized("clients.write", true, api.retryDeal))
	mux.HandleFunc("GET "+managerPath+"commands/{id}", managerAuthorized("", false, api.receipt))
	mux.HandleFunc("GET "+managerPath+"accounts", managerAuthorized("clients.read", false, api.accounts))
	mux.HandleFunc("GET "+managerPath+"accounts/{id}", managerAuthorized("clients.read", false, api.account))
	mux.HandleFunc("PATCH "+managerPath+"accounts/{id}/profile", managerAuthorized("clients.write", true, api.updateAccountProfile))
	mux.HandleFunc("GET "+managerPath+"accounts/{id}/crm-users", managerAuthorized("clients.read", false, api.crmUsers))
	mux.HandleFunc("GET "+managerPath+"accounts/{id}/license-history", managerAuthorized("clients.read", false, api.licenseHistory))
	mux.HandleFunc("GET "+managerPath+"accounts/{id}/widgets", managerAuthorized("clients.read", false, api.accountWidgets))
	mux.HandleFunc("POST "+managerPath+"accounts/{id}/refresh", managerAuthorized("clients.write", true, api.refreshAccount))
	mux.HandleFunc("GET "+managerPath+"operations/{id}", managerAuthorized("clients.write", false, api.operation))
	mux.HandleFunc("POST "+managerPath+"accounts/{id}/renewals", managerAuthorized("clients.renew", true, api.createAccountRenewal))
	registerManagerPortrait(mux, api)
	registerManagerProducts(mux, api)
	registerManagerCompanies(mux, api)
	mux.HandleFunc("GET "+managerPath+"feed", managerAuthorized("feed.read", false, api.feed))
	mux.HandleFunc("GET "+managerPath+"accounts/{id}/history", managerAuthorized("clients.read", false, api.accountHistory))
	return mux
}

type managerHTTP struct {
	store          managerStore
	options        *ManagerOptions
	accountGateway bool
}

type managerActor struct {
	Subject       uuid.UUID
	GrantsVersion int64
	Grants        map[string]bool
}

type managerEnvelope struct {
	Actor              managerActor
	RequestID, Command uuid.UUID
	Deadline           time.Time
}

func managerAuthorized(grant string, mutation bool, next func(http.ResponseWriter, *http.Request, managerEnvelope), legacyReadGrant ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		env, err := decodeManagerEnvelope(r.Header.Values("X-Platform-Actor"), now)
		if err != nil {
			writeManagerError(w, http.StatusUnauthorized, "authentication_required", uuid.Nil, 0)
			return
		}
		if grant != "" && !env.Actor.Grants[grant] &&
			(len(legacyReadGrant) == 0 || !env.Actor.Grants[legacyReadGrant[0]]) {
			writeManagerError(w, http.StatusForbidden, "permission_denied", env.RequestID, 0)
			return
		}
		if mutation && env.Command == uuid.Nil {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
		deadline := now.Add(5 * time.Second)
		if env.Deadline.Before(deadline) {
			deadline = env.Deadline
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		next(w, r.WithContext(ctx), env)
	}
}

func (m managerHTTP) list(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	filters, cursor, err := parseInstallationFilters(r.URL.RawQuery, m.options.zone)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	m.writeInstallations(w, r, env, filters, cursor)
}

func (m managerHTTP) writeInstallations(w http.ResponseWriter, r *http.Request, env managerEnvelope, filters installationFilters, cursor string) {
	var seek installationSeek
	var err error
	if cursor != "" {
		seek, err = m.options.parseCursor(cursor, env.Actor, filters.hash())
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.list(r.Context(), env.Actor, filters, seek)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	if next.Snapshot != uuid.Nil {
		cursor := m.options.cursor(next, env.Actor, filters.hash())
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func (m managerHTTP) retryDeal(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, idError := managerUUID(r.PathValue("id"))
	key, keyError := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, versionError := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	var reason string
	raw, bodyError := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if idError != nil || keyError != nil || versionError != nil || version > 2147483647 || r.URL.RawQuery != "" ||
		contentType != "application/json" || bodyError != nil || readManagerObject(raw, map[string]any{"reason": &reason}) != nil ||
		!managerText(reason, 1, 1000) {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	operation, err := m.store.retryInstallationDeal(r.Context(), dealRetryCommand{
		ID: env.Command, Subject: env.Actor.Subject, Key: key, Episode: id, Version: version, Reason: reason,
	})
	if err != nil {
		if errors.Is(err, errManagerConflict) {
			writeManagerError(w, http.StatusConflict, "version_conflict", env.RequestID, 0)
		} else {
			writeManagerStoreError(w, env.RequestID, err)
		}
		return
	}
	w.Header().Set("Location", managerPath+"operations/"+operation.ID.String())
	writeLifecycleJSON(w, http.StatusAccepted, operation)
}

func (m managerHTTP) review(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	episode, episodeErr := managerUUID(r.PathValue("id"))
	idempotency, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	version, versionErr := managerVersion(singleManagerHeader(r.Header, "If-Match"))
	contentType, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if episodeErr != nil || keyErr != nil || versionErr != nil || contentType != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	var input reviewInput
	var reviewed *bool
	if err := readManagerObject(raw, map[string]any{"reviewed": &reviewed, "reason": &input.Reason}); err != nil ||
		reviewed == nil || (input.Reason != nil && !managerText(*input.Reason, 1, 1000)) {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	input.Reviewed = *reviewed
	outcome, err := m.store.review(r.Context(), reviewCommand{
		ID: env.Command, Key: idempotency, Episode: episode, Subject: env.Actor.Subject,
		RequestID: env.RequestID, Version: version, Input: input,
	})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}

func (m managerHTTP) receipt(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	receipt, err := m.store.receipt(r.Context(), id, env.Actor)
	if errors.Is(err, errManagerNotFound) {
		op, lookup := m.store.readDealOperation(r.Context(), id, env.Actor.Subject)
		if lookup == nil {
			if !env.Actor.Grants["clients.write"] {
				writeManagerError(w, http.StatusForbidden, "permission_denied", env.RequestID, 0)
				return
			}
			receipt, err = managerReceipt{ID: id, Kind: dealRetryKind, State: "accepted", OperationID: &op.ID}, nil
		} else {
			err = lookup
		}
	}
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, http.StatusOK, receipt)
}

func decodeManagerEnvelope(headers []string, now time.Time) (managerEnvelope, error) {
	var env managerEnvelope
	if len(headers) != 1 || len(headers[0]) > 4096 {
		return env, errManagerInput
	}
	raw, err := managerBase64(headers[0])
	if err != nil {
		return env, errManagerInput
	}
	var actorRaw json.RawMessage
	var requestID, command *string
	var issued time.Time
	var trace string
	if err := readManagerObject(raw, map[string]any{
		"actor": &actorRaw, "request_id": &requestID, "command_id": &command, "trace_id": &trace, "issued_at": &issued, "deadline_at": &env.Deadline,
	}); err != nil || requestID == nil || issued.IsZero() || issued.After(now.Add(5*time.Second)) ||
		!env.Deadline.After(now) || !env.Deadline.After(issued) || env.Deadline.Sub(issued) > 30*time.Second {
		return env, errManagerInput
	}
	if env.RequestID, err = managerUUID(*requestID); err != nil {
		return env, errManagerInput
	}
	if command != nil {
		if env.Command, err = managerUUID(*command); err != nil {
			return env, errManagerInput
		}
	}

	var kind, audience, subject, session string
	var grants []string
	if err := readManagerObject(actorRaw, map[string]any{
		"kind": &kind, "audience": &audience, "subject_id": &subject, "session_id": &session,
		"grants": &grants, "grants_version": &env.Actor.GrantsVersion,
	}); err != nil || kind != "employee" || audience != "managers" ||
		env.Actor.GrantsVersion < 1 || env.Actor.GrantsVersion > wire.MaxPosition || len(grants) == 0 || len(grants) > 11 {
		return env, errManagerInput
	}
	if env.Actor.Subject, err = managerUUID(subject); err != nil {
		return env, errManagerInput
	}
	if _, err = managerUUID(session); err != nil {
		return env, errManagerInput
	}
	env.Actor.Grants = make(map[string]bool, len(grants))
	for _, grant := range grants {
		switch grant {
		case "catalog.manage", "catalog.read", "clients.read", "clients.renew", "clients.write", "configuration.manage",
			"feed.read", "identity.manage", "security.audit.read", "staff.manage", "stats.read":
		default:
			return env, errManagerInput
		}
		if env.Actor.Grants[grant] {
			return env, errManagerInput
		}
		env.Actor.Grants[grant] = true
	}
	return env, nil
}

var errManagerInput = errors.New("invalid manager input")

func readManagerObject(raw []byte, fields map[string]any) error {
	if !utf8.Valid(raw) {
		return errManagerInput
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return errManagerInput
	}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		dst, known := fields[name]
		if err != nil || !ok || !known {
			return errManagerInput
		}
		delete(fields, name)
		var member json.RawMessage
		_, nullable := dst.(*json.RawMessage)
		if err := decoder.Decode(&member); err != nil || (!nullable && bytes.Equal(member, []byte("null"))) || json.Unmarshal(member, dst) != nil {
			return errManagerInput
		}
	}
	if close, err := decoder.Token(); err != nil || close != json.Delim('}') {
		return errManagerInput
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errManagerInput
	}
	return nil
}

func singleManagerHeader(headers http.Header, name string) string {
	values := headers.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func managerUUID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || !strings.EqualFold(id.String(), raw) {
		return uuid.Nil, errManagerInput
	}
	return id, nil
}

func managerVersion(raw string) (int64, error) {
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, errManagerInput
	}
	number := raw[1 : len(raw)-1]
	version, err := strconv.ParseInt(number, 10, 64)
	if err != nil || version < 1 || version > wire.MaxPosition || strconv.FormatInt(version, 10) != number {
		return 0, errManagerInput
	}
	return version, nil
}

func managerText(value string, minimum, maximum int) bool {
	n := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && n >= minimum && n <= maximum
}

func managerBase64(raw string) ([]byte, error) {
	body, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(body) != raw {
		return nil, errManagerInput
	}
	return body, nil
}

func parseInstallationFilters(raw string, zone *time.Location) (installationFilters, string, error) {
	filters := installationFilters{Limit: 25, Zone: zone.String()}
	var cursor string
	if len(raw) > 4096 {
		return filters, "", errManagerInput
	}
	query, err := url.ParseQuery(raw)
	if err != nil {
		return filters, "", errManagerInput
	}
	for key, entries := range query {
		if len(entries) != 1 || !managerText(entries[0], 0, 200) {
			return filters, "", errManagerInput
		}
		value := entries[0]
		if value == "" && key != "q" {
			return filters, "", errManagerInput
		}
		switch key {
		case "cursor":
			cursor = value
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != value {
				return filters, "", errManagerInput
			}
			filters.Limit = n
		case "q":
			filters.Q = strings.ToLower(strings.TrimSpace(value))
		case "review":
			if value != "reviewed" && value != "unreviewed" {
				return filters, "", errManagerInput
			}
			filters.Review = value
		case "kind":
			if value != "install" && value != "reinstall" {
				return filters, "", errManagerInput
			}
			filters.Kind = value
		case "last_lifecycle_event":
			switch wire.Kind(value) {
			case wire.Installed, wire.Reinstalled, wire.Reauthorized, wire.Uninstalled:
			default:
				return filters, "", errManagerInput
			}
			filters.LastLifecycle = value
		case "product_id":
			id, err := managerUUID(value)
			if err != nil {
				return filters, "", errManagerInput
			}
			filters.Product = &id
		case "line":
			if value != "FREE" && value != "TOP" && value != "PRO" {
				return filters, "", errManagerInput
			}
			filters.Line = value
		case "activation":
			if value != "pending" && value != "activated" && value != "unavailable" {
				return filters, "", errManagerInput
			}
			filters.Activation = value
		case "access_state":
			if value != "active" && value != "inactive" {
				return filters, "", errManagerInput
			}
			filters.Access = value
		case "from", "to":
			day, err := time.ParseInLocation(time.DateOnly, value, zone)
			if err != nil || day.Year() == 0 || day.Format(time.DateOnly) != value {
				return filters, "", errManagerInput
			}
			if key == "from" {
				start := day.UTC()
				filters.From, filters.start = value, &start
			} else {
				before := day.AddDate(0, 0, 1).UTC()
				filters.To, filters.before = value, &before
			}
		case "sort":
			if value != "installed_at" && value != "-installed_at" {
				return filters, "", errManagerInput
			}
			filters.Ascending = value == "installed_at"
		default:
			return filters, "", errManagerInput
		}
	}
	if filters.From != "" && filters.To != "" && filters.From > filters.To {
		return filters, "", errManagerInput
	}
	return filters, cursor, nil
}

func (o ManagerOptions) cursor(seek installationSeek, actor managerActor, query [32]byte) string {
	var body [40]byte
	copy(body[:16], seek.Snapshot[:])
	binary.BigEndian.PutUint64(body[16:24], uint64(seek.At.UnixMicro()))
	copy(body[24:], seek.ID[:])
	return base64.RawURLEncoding.EncodeToString(append(body[:], o.cursorMAC(body[:], actor, query)...))
}

func (o ManagerOptions) parseCursor(raw string, actor managerActor, query [32]byte) (installationSeek, error) {
	body, err := managerBase64(raw)
	if err != nil || len(body) != 40+sha256.Size || !hmac.Equal(body[40:], o.cursorMAC(body[:40], actor, query)) {
		return installationSeek{}, errManagerCursor
	}
	return installationSeek{Snapshot: uuid.UUID(body[:16]), At: time.UnixMicro(int64(binary.BigEndian.Uint64(body[16:24]))),
		ID: uuid.UUID(body[24:40])}, nil
}

func (o ManagerOptions) cursorMAC(body []byte, actor managerActor, query [32]byte) []byte {
	return o.routeCursorMAC("platform-installations-v1", body, actor, query)
}

func (o ManagerOptions) routeCursorMAC(route string, body []byte, actor managerActor, query [32]byte) []byte {
	mac := hmac.New(sha256.New, o.cursorKey)
	mac.Write([]byte(route))
	mac.Write(actor.Subject[:])
	var grantsVersion [8]byte
	binary.BigEndian.PutUint64(grantsVersion[:], uint64(actor.GrantsVersion))
	mac.Write(grantsVersion[:])
	mac.Write(query[:])
	mac.Write(body)
	return mac.Sum(nil)
}

type managerErrorBody struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	RequestID  uuid.UUID `json:"request_id"`
	Retryable  bool      `json:"retryable"`
	Version    int64     `json:"current_version,omitempty"`
	RetryAfter int       `json:"retry_after_seconds,omitempty"`
}

type managerErrorResponse struct {
	Error managerErrorBody `json:"error"`
}

func managerProblem(status int, code string, requestID uuid.UUID, version int64, retryAfter int) managerErrorResponse {
	if requestID == uuid.Nil {
		requestID = uuid.New()
	}
	message := http.StatusText(status)
	switch code {
	case "invalid_cursor":
		message = "Start again without the cursor."
	case "query_too_broad":
		message = "Narrow the filters and try again."
	}
	return managerErrorResponse{Error: managerErrorBody{
		Code: code, Message: message, RequestID: requestID,
		Retryable: retryAfter > 0, Version: version, RetryAfter: retryAfter,
	}}
}

func writeManagerError(w http.ResponseWriter, status int, code string, requestID uuid.UUID, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeLifecycleJSON(w, status, managerProblem(status, code, requestID, 0, retryAfter))
}

func writeManagerStoreError(w http.ResponseWriter, requestID uuid.UUID, err error) {
	switch {
	case errors.Is(err, errManagerPermission):
		writeManagerError(w, http.StatusForbidden, "permission_denied", requestID, 0)
	case errors.Is(err, errManagerNotFound):
		writeManagerError(w, http.StatusNotFound, "resource_not_found", requestID, 0)
	case errors.Is(err, errManagerConflict):
		writeManagerError(w, http.StatusConflict, "idempotency_conflict", requestID, 0)
	case errors.Is(err, errManagerCursor):
		writeManagerError(w, http.StatusBadRequest, "invalid_cursor", requestID, 0)
	case errors.Is(err, errManagerQueryTooBroad):
		writeManagerError(w, http.StatusBadRequest, "query_too_broad", requestID, 0)
	case errors.Is(err, errManagerSnapshotLimit):
		writeManagerError(w, http.StatusTooManyRequests, "rate_limited", requestID, int(managerSnapshotTTL.Seconds()))
	default:
		writeManagerError(w, http.StatusServiceUnavailable, "dependency_unavailable", requestID, 1)
	}
}
