package adminread

import (
	"encoding/base64"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	defaultLimit           = 25
	maxLimit               = 100
	jobsWindow             = 7 * 24 * time.Hour
	defaultDeliveriesLimit = 100
)

type accountQuery struct {
	AccountID *int64
	Domain    string
	Subdomain string
}

func normalizeAccountQuery(raw string) accountQuery {
	q := strings.TrimSpace(raw)
	if q == "" {
		return accountQuery{}
	}
	lower := strings.ToLower(q)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if parsed, err := url.Parse(q); err == nil && parsed.Host != "" {
			host := parsed.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			q = host
		}
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return accountQuery{}
	}
	if isAllDigits(q) {
		id, err := strconv.ParseInt(q, 10, 64)
		if err == nil && id > 0 {
			return accountQuery{AccountID: &id}
		}
		return accountQuery{}
	}
	q = strings.ToLower(q)
	if strings.Contains(q, ".") {
		return accountQuery{Domain: q}
	}
	return accountQuery{Subdomain: q}
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func encodeCursor(at time.Time, id string) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(raw string) (time.Time, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", errInvalid("invalid cursor")
	}
	at, id, ok := strings.Cut(string(decoded), "|")
	if !ok || at == "" || id == "" {
		return time.Time{}, "", errInvalid("invalid cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", errInvalid("invalid cursor")
	}
	return ts.UTC(), id, nil
}

func parseLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errInvalid("invalid limit")
	}
	if value > maxLimit {
		return maxLimit, nil
	}
	return value, nil
}

func parseDeliveriesLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDeliveriesLimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, errInvalid("invalid limit")
	}
	if value > maxLimit {
		return maxLimit, nil
	}
	return value, nil
}

func parseJobsSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	now = now.UTC()
	if raw == "" {
		return now.Add(-jobsWindow), nil
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		since, err = time.Parse(time.RFC3339Nano, raw)
	}
	if err != nil {
		return time.Time{}, errInvalid("invalid since")
	}
	since = since.UTC()
	if now.Sub(since) > jobsWindow {
		return time.Time{}, errInvalid("jobs window exceeds 7 days")
	}
	return since, nil
}

func parseOptionalUUID(raw string) (*uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return nil, errInvalid("invalid identifier")
	}
	return &id, nil
}

func parsePathUUID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errInvalid("invalid identifier")
	}
	return id, nil
}

func parseAccountID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 1 {
		return 0, errInvalid("invalid account id")
	}
	return id, nil
}

func parseOptionalInt64(raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return nil, errInvalid("invalid account id")
	}
	return &id, nil
}
