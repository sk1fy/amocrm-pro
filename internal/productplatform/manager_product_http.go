package productplatform

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	wire "github.com/sk1fy/amocrm-pro/internal/platformlifecycle"
)

func registerManagerProducts(mux *http.ServeMux, m managerHTTP) {
	mux.HandleFunc("GET "+managerPath+"products", managerAuthorized("clients.read", false, m.listProducts))
	mux.HandleFunc("POST "+managerPath+"products", managerAuthorized("catalog.manage", true, m.createProduct))
	mux.HandleFunc("GET "+managerPath+"products/{id}", managerAuthorized("clients.read", false, m.getProduct))
	mux.HandleFunc("PATCH "+managerPath+"products/{id}", managerAuthorized("catalog.manage", true, m.updateProduct))
	mux.HandleFunc("GET "+managerPath+"products/{id}/metrics", managerAuthorized("stats.read", false, m.getProductMetrics))
}

func parseProductFilters(raw string) (productFilters, string, error) {
	f := productFilters{Limit: 25}
	values, err := boundedAccountQuery(raw)
	if err != nil {
		return f, "", err
	}
	var cursor string
	for key, entries := range values {
		if len(entries) != 1 || !managerText(entries[0], 0, 1000) {
			return f, "", errManagerInput
		}
		value := entries[0]
		switch key {
		case "q":
			if !managerText(value, 0, 200) {
				return f, "", errManagerInput
			}
			f.Q = value
		case "line":
			if !productLine(value) {
				return f, "", errManagerInput
			}
			f.Line = value
		case "availability":
			if !productAvailability(value) {
				return f, "", errManagerInput
			}
			f.Availability = value
		case "limit":
			f.Limit, err = accountInteger(value, 1, 100)
			if err != nil {
				return f, "", err
			}
		case "cursor":
			if value == "" {
				return f, "", errManagerInput
			}
			cursor = value
		default:
			return f, "", errManagerInput
		}
	}
	return f, cursor, nil
}

func (m managerHTTP) listProducts(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	f, cursor, err := parseProductFilters(r.URL.RawQuery)
	if err != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	var after productSeek
	if cursor != "" {
		after, err = m.options.parseProductCursor(cursor, env.Actor, f.hash())
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	page, next, err := m.store.products(r.Context(), f, after)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	if next.ID != uuid.Nil {
		cursor := m.options.productCursor(next, env.Actor, f.hash())
		page.Next = &cursor
	}
	writeLifecycleJSON(w, http.StatusOK, page)
}

func (o ManagerOptions) productCursor(after productSeek, actor managerActor, query [32]byte) string {
	body, _ := json.Marshal(after)
	return base64.RawURLEncoding.EncodeToString(append(body, o.routeCursorMAC("platform-products-v1", body, actor, query)...))
}
func (o ManagerOptions) parseProductCursor(raw string, actor managerActor, query [32]byte) (productSeek, error) {
	var after productSeek
	body, err := managerBase64(raw)
	if err != nil || len(body) <= 32 || len(body) > 400 {
		return after, errManagerCursor
	}
	payload := body[:len(body)-32]
	if !hmac.Equal(body[len(body)-32:], o.routeCursorMAC("platform-products-v1", payload, actor, query)) || json.Unmarshal(payload, &after) != nil ||
		after.ID == uuid.Nil || after.Revision < 1 || !wire.ValidProductCode(after.Code) || after.SnapshotAt.IsZero() {
		return productSeek{}, errManagerCursor
	}
	return after, nil
}

func (m managerHTTP) getProduct(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	if err != nil || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	p, err := m.store.product(r.Context(), id)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	w.Header().Set("ETag", managerETag(p.Version))
	writeLifecycleJSON(w, http.StatusOK, p)
}

func (m managerHTTP) createProduct(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	m.writeProduct(w, r, env, true)
}
func (m managerHTTP) updateProduct(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	m.writeProduct(w, r, env, false)
}

func (m managerHTTP) writeProduct(w http.ResponseWriter, r *http.Request, env managerEnvelope, create bool) {
	key, keyErr := managerUUID(singleManagerHeader(r.Header, "Idempotency-Key"))
	media, _, _ := mime.ParseMediaType(singleManagerHeader(r.Header, "Content-Type"))
	if keyErr != nil || media != "application/json" || r.URL.RawQuery != "" {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	cmd := managerCommand{ID: env.Command, Key: key, Subject: env.Actor.Subject, RequestID: env.RequestID}
	if create {
		cmd.Kind = productCreateCommandKind
	} else {
		var err error
		cmd.Kind = productUpdateCommandKind
		cmd.Product, err = managerUUID(r.PathValue("id"))
		if err != nil {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
		cmd.Version, err = managerVersion(singleManagerHeader(r.Header, "If-Match"))
		if err != nil {
			writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
			return
		}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	input, inputErr := parseProductInput(raw, create)
	if err != nil || inputErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	if create {
		cmd.Resource, err = m.store.productTarget(r.Context(), input.Code)
		if err != nil {
			writeManagerStoreError(w, env.RequestID, err)
			return
		}
	}
	outcome, err := m.store.mutate(r.Context(), cmd, input, func(ctx context.Context, tx pgx.Tx) (managerOutcome, error) {
		changes, err := tx.Begin(ctx)
		if err != nil {
			return managerOutcome{}, err
		}
		defer func() { _ = changes.Rollback(ctx) }()
		outcome, err := saveProduct(ctx, changes, cmd, input, create)
		if err != nil || outcome.status >= 400 {
			return outcome, err
		}
		return outcome, changes.Commit(ctx)
	})
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeManagerOutcome(w, outcome)
}

func (m managerHTTP) getProductMetrics(w http.ResponseWriter, r *http.Request, env managerEnvelope) {
	id, err := managerUUID(r.PathValue("id"))
	start, before, dateErr := parseProductMetricDates(r.URL.RawQuery, m.options.zone)
	if err != nil || dateErr != nil {
		writeManagerError(w, http.StatusBadRequest, "validation_failed", env.RequestID, 0)
		return
	}
	metrics, err := m.store.productMetrics(r.Context(), id, start, before)
	if err != nil {
		writeManagerStoreError(w, env.RequestID, err)
		return
	}
	writeLifecycleJSON(w, http.StatusOK, metrics)
}

func parseProductMetricDates(raw string, zone *time.Location) (*time.Time, *time.Time, error) {
	values, err := boundedAccountQuery(raw)
	if err != nil {
		return nil, nil, err
	}
	var start, before *time.Time
	for key, entries := range values {
		if len(entries) != 1 || key != "from" && key != "to" {
			return nil, nil, errManagerInput
		}
		day, err := time.ParseInLocation(time.DateOnly, entries[0], zone)
		if err != nil || day.Year() < 1 || day.Format(time.DateOnly) != entries[0] {
			return nil, nil, errManagerInput
		}
		if key == "from" {
			utc := day.UTC()
			start = &utc
		} else {
			utc := day.AddDate(0, 0, 1).UTC()
			before = &utc
		}
	}
	if start != nil && before != nil && !start.Before(*before) {
		return nil, nil, errManagerInput
	}
	return start, before, nil
}
