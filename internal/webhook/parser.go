package webhook

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var eventFieldPattern = regexp.MustCompile(`^([^\[]+)\[([^\]]+)\]\[(\d+)\]\[(.+)\]$`)

var ErrNoEvents = errors.New("webhook payload contains no supported events")

var ErrPayloadComplexity = errors.New("webhook payload exceeds event complexity limits")

const (
	maxFormKeys       = 5_000
	maxEvents         = 100
	maxFieldsPerEvent = 200
	maxValuesPerField = 20
)

var defaultAllowedEvents = []string{
	"add_lead", "update_lead", "status_lead", "delete_lead",
	"add_contact", "update_contact",
}

type Event struct {
	EntityType       string
	EventType        string
	EntityID         *int64
	EventAt          *time.Time
	Payload          json.RawMessage
	DeduplicationKey []byte
}

type groupedEvent struct {
	entityType string
	eventType  string
	index      int
	fields     map[string]any
}

func AccountID(raw []byte) (int64, bool) {
	for len(raw) > 0 {
		pair := raw
		if delimiter := bytes.IndexByte(raw, '&'); delimiter >= 0 {
			pair = raw[:delimiter]
			raw = raw[delimiter+1:]
		} else {
			raw = nil
		}
		separator := bytes.IndexByte(pair, '=')
		if separator < 0 {
			continue
		}
		key, err := url.QueryUnescape(string(pair[:separator]))
		if err != nil || key != "account[id]" {
			continue
		}
		value, err := url.QueryUnescape(string(pair[separator+1:]))
		if err != nil {
			return 0, false
		}
		accountID, err := strconv.ParseInt(value, 10, 64)
		return accountID, err == nil && accountID > 0
	}
	return 0, false
}

func Parse(installationID uuid.UUID, raw []byte) ([]Event, error) {
	return ParseAllowed(installationID, raw, defaultAllowedEvents)
}

func ParseAllowed(installationID uuid.UUID, raw []byte, allowedEvents []string) ([]Event, error) {
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}
	if len(values) > maxFormKeys {
		return nil, ErrPayloadComplexity
	}
	allowed := make(map[string]struct{}, len(allowedEvents))
	for _, event := range allowedEvents {
		allowed[event] = struct{}{}
	}

	groups := make(map[string]*groupedEvent)
	sawCandidate := false
	for key, formValues := range values {
		matches := eventFieldPattern.FindStringSubmatch(key)
		if len(matches) != 5 || matches[1] == "account" {
			continue
		}
		sawCandidate = true
		if _, ok := allowed[eventSetting(matches[1], matches[2])]; !ok {
			continue
		}
		if len(formValues) > maxValuesPerField {
			return nil, ErrPayloadComplexity
		}
		index, err := strconv.Atoi(matches[3])
		if err != nil {
			continue
		}
		groupKey := matches[1] + "\x00" + matches[2] + "\x00" + matches[3]
		group, ok := groups[groupKey]
		if !ok {
			if len(groups) >= maxEvents {
				return nil, ErrPayloadComplexity
			}
			group = &groupedEvent{
				entityType: matches[1],
				eventType:  matches[2],
				index:      index,
				fields:     make(map[string]any),
			}
			groups[groupKey] = group
		}
		if _, exists := group.fields[matches[4]]; !exists && len(group.fields) >= maxFieldsPerEvent {
			return nil, ErrPayloadComplexity
		}
		if len(formValues) == 1 {
			group.fields[matches[4]] = formValues[0]
		} else {
			copied := append([]string(nil), formValues...)
			sort.Strings(copied)
			group.fields[matches[4]] = copied
		}
	}

	ordered := make([]*groupedEvent, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].entityType != ordered[j].entityType {
			return ordered[i].entityType < ordered[j].entityType
		}
		if ordered[i].eventType != ordered[j].eventType {
			return ordered[i].eventType < ordered[j].eventType
		}
		return ordered[i].index < ordered[j].index
	})

	events := make([]Event, 0, len(ordered))
	for _, group := range ordered {
		payload, err := json.Marshal(group.fields)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized webhook event: %w", err)
		}
		entityID := positiveInt64(group.fields["id"])
		eventAt := eventTime(group.fields)
		events = append(events, Event{
			EntityType:       group.entityType,
			EventType:        group.eventType,
			EntityID:         entityID,
			EventAt:          eventAt,
			Payload:          payload,
			DeduplicationKey: deduplicationKey(installationID, group.entityType, group.eventType, entityID, eventAt, payload),
		})
	}
	if len(events) == 0 && !sawCandidate {
		return nil, ErrNoEvents
	}
	return events, nil
}

func eventSetting(entityType, eventType string) string {
	entity := strings.TrimSuffix(entityType, "s")
	if entityType == "companies" {
		entity = "company"
	}
	return eventType + "_" + entity
}

func positiveInt64(value any) *int64 {
	raw, ok := value.(string)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return nil
	}
	return &parsed
}

func eventTime(fields map[string]any) *time.Time {
	for _, field := range []string{"last_modified", "updated_at", "created_at", "timestamp"} {
		value := positiveInt64(fields[field])
		if value == nil {
			continue
		}
		parsed := time.Unix(*value, 0).UTC()
		if parsed.Before(time.Unix(0, 0).UTC()) || parsed.After(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) {
			return nil
		}
		return &parsed
	}
	return nil
}

func deduplicationKey(
	installationID uuid.UUID,
	entityType string,
	eventType string,
	entityID *int64,
	eventAt *time.Time,
	payload json.RawMessage,
) []byte {
	payloadHash := sha256.Sum256(payload)
	parts := []string{installationID.String(), entityType, eventType, "", ""}
	if entityID != nil {
		parts[3] = strconv.FormatInt(*entityID, 10)
	}
	if eventAt != nil {
		parts[4] = strconv.FormatInt(eventAt.Unix(), 10)
	}
	input := strings.Join(parts, "\x00") + "\x00" + fmt.Sprintf("%x", payloadHash[:])
	sum := sha256.Sum256([]byte(input))
	return sum[:]
}
