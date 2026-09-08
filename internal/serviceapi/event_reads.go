package serviceapi

import "context"

// EventReadVersion certifies support for filters, ordering and compact views.
// New callers reject old owners that would otherwise silently ignore new fields.
const EventReadVersion = 2

type EventRequest struct {
	Auth    Auth   `json:"auth"`
	EventID string `json:"event_id"`
}

// EventReader is an additive port: legacy consumers of CRMEvents keep their
// existing interface. An unavailable detail port is an error, never a fallback
// to unscoped list reads or a direct amoCRM request.
type EventReader interface {
	GetEvent(context.Context, EventRequest) (Event, error)
}

func ValidateEventRequest(r EventRequest) error {
	if !eventIdentifier(r.EventID, 128) {
		return Fail(InvalidArgument, "event_id must be a bounded event identifier")
	}
	return nil
}

func eventIdentifier(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit {
		return false
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func validateEventFilters(q Query) error {
	if len(q.Types) > 32 || len(q.EntityIDs) > 100 || len(q.Categories) > 11 || len(q.DirectoryUserIDs) > 100 || q.GroupID < 0 || q.Order != "" && q.Order != "asc" && q.Order != "desc" {
		return Fail(InvalidArgument, "event filters exceed bounds or order is invalid")
	}
	seen := map[string]bool{}
	for _, kind := range q.Types {
		if !eventIdentifier(kind, 128) || seen[kind] {
			return Fail(InvalidArgument, "types must be distinct bounded event types")
		}
		seen[kind] = true
	}
	if q.TypePrefix != "" && !eventIdentifier(q.TypePrefix, 64) || q.EntityType != "" && !eventIdentifier(q.EntityType, 64) {
		return Fail(InvalidArgument, "invalid type prefix or entity type")
	}
	if len(q.EntityIDs) > 0 && q.EntityType == "" {
		return Fail(InvalidArgument, "entity_ids requires entity_type")
	}
	ids := map[int64]bool{}
	for _, id := range q.EntityIDs {
		if id <= 0 || ids[id] {
			return Fail(InvalidArgument, "entity_ids must be distinct positive IDs")
		}
		ids[id] = true
	}
	cats := map[string]bool{}
	for _, category := range q.Categories {
		if !ValidCategory(category) || cats[category] {
			return Fail(InvalidArgument, "categories must be distinct known product families")
		}
		cats[category] = true
	}
	dir := map[int64]bool{}
	for _, id := range q.DirectoryUserIDs {
		if id <= 0 || dir[id] {
			return Fail(InvalidArgument, "directory_user_ids must be distinct positive IDs")
		}
		dir[id] = true
	}
	if q.Timezone != "" && !validTimezone(q.Timezone) {
		return Fail(InvalidArgument, "invalid timezone")
	}
	if q.Buckets != "" && q.Buckets != BucketNone && q.Buckets != BucketHour && q.Buckets != BucketDay && q.Buckets != BucketAuto {
		return Fail(InvalidArgument, "buckets must be hour, day, auto or none")
	}
	return nil
}

func HasExtendedQuery(q Query) bool {
	return len(q.Types) > 0 || q.TypePrefix != "" || q.EntityType != "" || len(q.EntityIDs) > 0 || q.Order != "" || q.Compact || q.Cursor != "" || HasPresentationQuery(q)
}

func RequireQueryVersion(q Query, result QueryResult) error {
	if HasExtendedQuery(q) && result.ReadVersion < EventReadVersion || result.PayloadsOmitted != q.Compact {
		return Fail(Unavailable, "event reader does not support requested filters or view; update service versions")
	}
	if HasPresentationQuery(q) && result.ReadVersion < PresentationReadVersion {
		return Fail(Unavailable, "event reader does not support presentation filters or aggregates; update service versions")
	}
	return nil
}
